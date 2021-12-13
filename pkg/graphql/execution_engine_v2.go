package graphql

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"

	lru "github.com/hashicorp/golang-lru"
	"github.com/jensneuse/abstractlogger"

	"github.com/jensneuse/graphql-go-tools/pkg/ast"
	"github.com/jensneuse/graphql-go-tools/pkg/astprinter"
	"github.com/jensneuse/graphql-go-tools/pkg/engine/datasource/httpclient"
	"github.com/jensneuse/graphql-go-tools/pkg/engine/plan"
	"github.com/jensneuse/graphql-go-tools/pkg/engine/resolve"
	"github.com/jensneuse/graphql-go-tools/pkg/operationreport"
	"github.com/jensneuse/graphql-go-tools/pkg/pool"
	"github.com/jensneuse/graphql-go-tools/pkg/postprocess"
)

type EngineResultWriter struct {
	buf           *bytes.Buffer
	flushCallback func(data []byte)
}

func NewEngineResultWriter() EngineResultWriter {
	return EngineResultWriter{
		buf: &bytes.Buffer{},
	}
}

func NewEngineResultWriterFromBuffer(buf *bytes.Buffer) EngineResultWriter {
	return EngineResultWriter{
		buf: buf,
	}
}

func (e *EngineResultWriter) SetFlushCallback(flushCb func(data []byte)) {
	e.flushCallback = flushCb
}

func (e *EngineResultWriter) Write(p []byte) (n int, err error) {
	return e.buf.Write(p)
}

func (e *EngineResultWriter) Read(p []byte) (n int, err error) {
	return e.buf.Read(p)
}

func (e *EngineResultWriter) Flush() {
	if e.flushCallback != nil {
		e.flushCallback(e.Bytes())
	}

	e.Reset()
}

func (e *EngineResultWriter) Len() int {
	return e.buf.Len()
}

func (e *EngineResultWriter) Bytes() []byte {
	return e.buf.Bytes()
}

func (e *EngineResultWriter) String() string {
	return e.buf.String()
}

func (e *EngineResultWriter) Reset() {
	e.buf.Reset()
}

func (e *EngineResultWriter) AsHTTPResponse(status int, headers http.Header) *http.Response {
	b := &bytes.Buffer{}

	switch headers.Get(httpclient.ContentEncodingHeader) {
	case "gzip":
		gzw := gzip.NewWriter(b)
		_, _ = gzw.Write(e.Bytes())
		_ = gzw.Close()
	case "deflate":
		fw, _ := flate.NewWriter(b, 1)
		_, _ = fw.Write(e.Bytes())
		_ = fw.Close()
	default:
		headers.Del(httpclient.ContentEncodingHeader) // delete unsupported compression header
		b = e.buf
	}

	res := &http.Response{}
	res.Body = ioutil.NopCloser(b)
	res.Header = headers
	res.StatusCode = status
	res.ContentLength = int64(b.Len())
	res.Header.Set("Content-Length", strconv.Itoa(b.Len()))
	return res
}

type internalExecutionContext struct {
	resolveContext *resolve.Context
	postProcessor  *postprocess.Processor
}

func newInternalExecutionContext() *internalExecutionContext {
	return &internalExecutionContext{
		resolveContext: resolve.NewContext(context.Background()),
		postProcessor:  postprocess.DefaultProcessor(),
	}
}

func (e *internalExecutionContext) prepare(ctx context.Context, operationRequest *Request) {
	e.setContext(ctx)
	e.setOperationRequest(operationRequest)
}

func (e *internalExecutionContext) setOperationRequest(operationRequest *Request) {
	e.resolveContext.Variables = operationRequest.Variables
	e.resolveContext.OperationDocument, _ = operationRequest.OperationDocument()
	e.resolveContext.OperationName = operationRequest.OperationName
	e.resolveContext.Request = operationRequest.request
}

func (e *internalExecutionContext) setContext(ctx context.Context) {
	e.resolveContext.Context = ctx
}

func (e *internalExecutionContext) reset() {
	e.resolveContext.Free()
}

type ExecutionEngineV2 struct {
	logger                       abstractlogger.Logger
	config                       EngineV2Configuration
	planner                      *plan.Planner
	plannerMu                    sync.Mutex
	resolver                     *resolve.Resolver
	internalExecutionContextPool sync.Pool
	executionPlanCache           *lru.Cache
	operationMiddleware          OperationMiddleware
	//rootFieldMiddleware          resolve.RootFieldMiddleware
}

type WebsocketBeforeStartHook interface {
	OnBeforeStart(reqCtx context.Context, operation *Request) error
}

type ExecutionOptionsV2 func(ctx *internalExecutionContext)

func WithBeforeFetchHook(hook resolve.BeforeFetchHook) ExecutionOptionsV2 {
	return func(ctx *internalExecutionContext) {
		ctx.resolveContext.SetBeforeFetchHook(hook)
	}
}

func WithAfterFetchHook(hook resolve.AfterFetchHook) ExecutionOptionsV2 {
	return func(ctx *internalExecutionContext) {
		ctx.resolveContext.SetAfterFetchHook(hook)
	}
}

type OperationHandler func(ctx context.Context, operation *Request, writer resolve.FlushWriter) error
type OperationMiddleware func(next OperationHandler) OperationHandler

func NewExecutionEngineV2(ctx context.Context, logger abstractlogger.Logger, engineConfig EngineV2Configuration) (*ExecutionEngineV2, error) {
	executionPlanCache, err := lru.New(1024)
	if err != nil {
		return nil, err
	}
	fetcher := resolve.NewFetcher(engineConfig.dataLoaderConfig.EnableSingleFlightLoader)
	rootFieldMiddleware := processRootFieldMiddleware(engineConfig.rootFieldMiddleware...)
	resolver := resolve.New(ctx, fetcher, engineConfig.dataLoaderConfig.EnableDataLoader)
	resolver.SetRootFieldMiddleware(rootFieldMiddleware)

	return &ExecutionEngineV2{
		logger:   logger,
		config:   engineConfig,
		planner:  plan.NewPlanner(ctx, engineConfig.plannerConfig),
		resolver: resolver,
		internalExecutionContextPool: sync.Pool{
			New: func() interface{} {
				return newInternalExecutionContext()
			},
		},
		executionPlanCache:  executionPlanCache,
		operationMiddleware: processOperationMiddleware(),
	}, nil
}

func (e *ExecutionEngineV2) Execute(ctx context.Context, operation *Request, writer resolve.FlushWriter, options ...ExecutionOptionsV2) error {
	if !operation.IsNormalized() {
		result, err := operation.Normalize(e.config.schema)
		if err != nil {
			return err
		}

		if !result.Successful {
			return result.Errors
		}
	}

	result, err := operation.ValidateForSchema(e.config.schema)
	if err != nil {
		return err
	}
	if !result.Valid {
		return result.Errors
	}

	operationHandler := e.operationMiddleware(func(ctx context.Context, operation *Request, writer resolve.FlushWriter) error {
		execContext := e.getExecutionCtx()
		defer e.putExecutionCtx(execContext)

		execContext.prepare(ctx, operation)

		for i := range options {
			options[i](execContext)
		}

		var report operationreport.Report
		cachedPlan := e.getCachedPlan(execContext, &operation.document, &e.config.schema.document, operation.OperationName, &report)
		if report.HasErrors() {
			return report
		}

		switch p := cachedPlan.(type) {
		case *plan.SynchronousResponsePlan:
			err = e.resolver.ResolveGraphQLResponse(execContext.resolveContext, p.Response, nil, writer)
		case *plan.SubscriptionResponsePlan:
			err = e.resolver.ResolveGraphQLSubscription(execContext.resolveContext, p.Response, writer)
		default:
			return errors.New("execution of operation is not possible")
		}

		return err
	})

	return operationHandler(ctx, operation, writer)
}

func (e *ExecutionEngineV2) UseOperation(mw OperationMiddleware) {
	e.operationMiddleware = processOperationMiddleware(e.operationMiddleware, mw)
}

func (e *ExecutionEngineV2) getCachedPlan(ctx *internalExecutionContext, operation, definition *ast.Document, operationName string, report *operationreport.Report) plan.Plan {

	hash := pool.Hash64.Get()
	hash.Reset()
	defer pool.Hash64.Put(hash)
	err := astprinter.Print(operation, definition, hash)
	if err != nil {
		report.AddInternalError(err)
		return nil
	}

	cacheKey := hash.Sum64()

	if cached, ok := e.executionPlanCache.Get(cacheKey); ok {
		if p, ok := cached.(plan.Plan); ok {
			return p
		}
	}

	e.plannerMu.Lock()
	defer e.plannerMu.Unlock()
	planResult := e.planner.Plan(operation, definition, operationName, report)
	if report.HasErrors() {
		return nil
	}

	p := ctx.postProcessor.Process(planResult)
	e.executionPlanCache.Add(cacheKey, p)
	return p
}

func (e *ExecutionEngineV2) GetWebsocketBeforeStartHook() WebsocketBeforeStartHook {
	return e.config.websocketBeforeStartHook
}

func (e *ExecutionEngineV2) getExecutionCtx() *internalExecutionContext {
	return e.internalExecutionContextPool.Get().(*internalExecutionContext)
}

func (e *ExecutionEngineV2) putExecutionCtx(ctx *internalExecutionContext) {
	ctx.reset()
	e.internalExecutionContextPool.Put(ctx)
}

func processOperationMiddleware(middlewares ...OperationMiddleware) OperationMiddleware {
	middleware := OperationMiddleware(func(next OperationHandler) OperationHandler {
		return next
	})

	// the first middleware is the outer most middleware and runs first.
	for i := len(middlewares) - 1; i >= 0; i-- {
		previousMW := middleware
		currentMW := middlewares[i]
		middleware = func(next OperationHandler) OperationHandler {
			return previousMW(currentMW(next))
		}
	}

	return middleware
}

func processRootFieldMiddleware(middlewares ...resolve.RootFieldMiddleware) resolve.RootFieldMiddleware {
	middleware := resolve.RootFieldMiddleware(func(next resolve.RootResolver) resolve.RootResolver {
		return next
	})

	// the first middleware is the outer most middleware and runs first.
	for i := len(middlewares) - 1; i >= 0; i-- {
		previousMW := middleware
		currentMW := middlewares[i]
		middleware = func(next resolve.RootResolver) resolve.RootResolver {
			return previousMW(currentMW(next))
		}
	}

	return middleware
}
