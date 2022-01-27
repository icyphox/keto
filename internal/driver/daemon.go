package driver

import (
	"context"
	"net"
	"net/http"
	"strings"

	grpcMiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/julienschmidt/httprouter"
	"github.com/ory/herodot"
	"github.com/ory/x/reqlog"
	"github.com/rs/cors"
	"github.com/urfave/negroni"
	grpcHealthV1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/ory/keto/internal/x"
	acl "github.com/ory/keto/proto/ory/keto/acl/v1alpha1"

	"github.com/ory/keto/internal/check"
	"github.com/ory/keto/internal/expand"
	"github.com/ory/keto/internal/relationtuple"

	"github.com/ory/analytics-go/v4"
	"github.com/ory/x/healthx"
	"github.com/ory/x/metricsx"
	"github.com/spf13/cobra"

	"github.com/ory/keto/internal/driver/config"

	"github.com/ory/graceful"
	"github.com/pkg/errors"
	"github.com/soheilhy/cmux"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

func (r *RegistryDefault) enableSqa(cmd *cobra.Command) {
	ctx := cmd.Context()

	r.sqaService = metricsx.New(
		cmd,
		r.Logger(),
		r.Config(ctx).Source(),
		&metricsx.Options{
			Service:       "ory-keto",
			ClusterID:     metricsx.Hash(r.Config(ctx).DSN()),
			IsDevelopment: strings.HasPrefix(r.Config(ctx).DSN(), "sqlite"),
			WriteKey:      "qQlI6q8Q4WvkzTjKQSor4sHYOikHIvvi",
			WhitelistedPaths: []string{
				"/",
				healthx.AliveCheckPath,
				healthx.ReadyCheckPath,
				healthx.VersionPath,

				relationtuple.RouteBase,
				check.RouteBase,
				expand.RouteBase,
			},
			BuildVersion: config.Version,
			BuildHash:    config.Commit,
			BuildTime:    config.Date,
			Config: &analytics.Config{
				Endpoint: "https://sqa.ory.sh",
			},
		},
	)
}

func (r *RegistryDefault) ServeAllSQA(cmd *cobra.Command) error {
	r.enableSqa(cmd)
	return r.ServeAll(cmd.Context())
}

func (r *RegistryDefault) ServeAll(ctx context.Context) error {
	eg := &errgroup.Group{}

	eg.Go(r.ServeRead(ctx))
	eg.Go(r.ServeWrite(ctx))

	return eg.Wait()
}

func (r *RegistryDefault) ServeRead(ctx context.Context) func() error {
	rt, s := r.ReadRouter(ctx), r.ReadGRPCServer(ctx)

	return func() error {
		return multiplexPort(ctx, r.Config(ctx).ReadAPIListenOn(), rt, s)
	}
}

func (r *RegistryDefault) ServeWrite(ctx context.Context) func() error {
	rt, s := r.WriteRouter(ctx), r.WriteGRPCServer(ctx)

	return func() error {
		return multiplexPort(ctx, r.Config(ctx).WriteAPIListenOn(), rt, s)
	}
}

func multiplexPort(ctx context.Context, addr string, router http.Handler, grpcS *grpc.Server) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	m := cmux.New(l)
	m.SetReadTimeout(graceful.DefaultReadTimeout)

	grpcL := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	httpL := m.Match(cmux.HTTP1())

	restS := graceful.WithDefaults(&http.Server{
		Handler: router,
	})

	eg := &errgroup.Group{}
	ctx, cancel := context.WithCancel(ctx)
	serversDone := make(chan struct{}, 2)

	eg.Go(func() error {
		defer func() {
			serversDone <- struct{}{}
		}()
		return errors.WithStack(grpcS.Serve(grpcL))
	})

	eg.Go(func() error {
		defer func() {
			serversDone <- struct{}{}
		}()
		if err := restS.Serve(httpL); !errors.Is(err, http.ErrServerClosed) {
			// unexpected error
			return errors.WithStack(err)
		}
		return nil
	})

	eg.Go(func() error {
		err := m.Serve()
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			// unexpected error
			return errors.WithStack(err)
		}
		// trigger further shutdown
		cancel()
		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()

		m.Close()
		for i := 0; i < 2; i++ {
			<-serversDone
		}

		// we have to stop the servers as well as they might still be running (for whatever reason I could not figure out)
		grpcS.GracefulStop()

		ctx, cancel := context.WithTimeout(context.Background(), graceful.DefaultReadTimeout)
		defer cancel()
		return restS.Shutdown(ctx)
	})

	if err := eg.Wait(); !errors.Is(err, cmux.ErrServerClosed) &&
		!errors.Is(err, cmux.ErrListenerClosed) &&
		(err != nil && !strings.Contains(err.Error(), "use of closed network connection")) {
		// unexpected error
		return err
	}
	return nil
}

func (r *RegistryDefault) allHandlers() []Handler {
	if len(r.handlers) == 0 {
		r.handlers = []Handler{
			relationtuple.NewHandler(r),
			check.NewHandler(r),
			expand.NewHandler(r),
		}
	}
	return r.handlers
}

func (r *RegistryDefault) ReadRouter(ctx context.Context) http.Handler {
	n := negroni.New()
	for _, f := range r.defaultHttpMiddlewares {
		n.UseFunc(f)
	}
	n.Use(reqlog.NewMiddlewareFromLogger(r.l, "read#Ory Keto").ExcludePaths(healthx.AliveCheckPath, healthx.ReadyCheckPath))

	br := &x.ReadRouter{Router: httprouter.New()}

	r.HealthHandler().SetHealthRoutes(br.Router, false)
	r.HealthHandler().SetVersionRoutes(br.Router)

	for _, h := range r.allHandlers() {
		h.RegisterReadRoutes(br)
	}

	n.UseHandler(br)

	if t := r.Tracer(ctx); t.IsLoaded() {
		n.Use(t)
	}

	if r.sqaService != nil {
		n.Use(r.sqaService)
	}

	var handler http.Handler = n
	options, enabled := r.Config(ctx).CORS("read")
	if enabled {
		handler = cors.New(options).Handler(handler)
	}

	return handler
}

func (r *RegistryDefault) WriteRouter(ctx context.Context) http.Handler {
	n := negroni.New()
	for _, f := range r.defaultHttpMiddlewares {
		n.UseFunc(f)
	}
	n.Use(reqlog.NewMiddlewareFromLogger(r.l, "write#Ory Keto").ExcludePaths(healthx.AliveCheckPath, healthx.ReadyCheckPath))

	pr := &x.WriteRouter{Router: httprouter.New()}

	r.HealthHandler().SetHealthRoutes(pr.Router, false)
	r.HealthHandler().SetVersionRoutes(pr.Router)

	for _, h := range r.allHandlers() {
		h.RegisterWriteRoutes(pr)
	}

	n.UseHandler(pr)

	if t := r.Tracer(ctx); t.IsLoaded() {
		n.Use(t)
	}

	if r.sqaService != nil {
		n.Use(r.sqaService)
	}

	var handler http.Handler = n
	options, enabled := r.Config(ctx).CORS("write")
	if enabled {
		handler = cors.New(options).Handler(handler)
	}

	return handler
}

func (r *RegistryDefault) unaryInterceptors(ctx context.Context) []grpc.UnaryServerInterceptor {
	is := make([]grpc.UnaryServerInterceptor, len(r.defaultUnaryInterceptors), len(r.defaultUnaryInterceptors)+2)
	copy(is, r.defaultUnaryInterceptors)
	is = append(is,
		herodot.UnaryErrorUnwrapInterceptor,
		grpcMiddleware.ChainUnaryServer(
			grpc_logrus.UnaryServerInterceptor(r.l.Entry),
		),
	)
	if r.Tracer(ctx).IsLoaded() {
		is = append(is, otgrpc.OpenTracingServerInterceptor(r.Tracer(ctx).Tracer()))
	}
	if r.sqaService != nil {
		is = append(is, r.sqaService.UnaryInterceptor)
	}
	return is
}

func (r *RegistryDefault) streamInterceptors(ctx context.Context) []grpc.StreamServerInterceptor {
	is := make([]grpc.StreamServerInterceptor, len(r.defaultStreamInterceptors), len(r.defaultStreamInterceptors)+2)
	copy(is, r.defaultStreamInterceptors)
	is = append(is,
		herodot.StreamErrorUnwrapInterceptor,
		grpcMiddleware.ChainStreamServer(
			grpc_logrus.StreamServerInterceptor(r.l.Entry),
		),
	)
	if r.Tracer(ctx).IsLoaded() {
		is = append(is, otgrpc.OpenTracingStreamServerInterceptor(r.Tracer(ctx).Tracer()))
	}
	if r.sqaService != nil {
		is = append(is, r.sqaService.StreamInterceptor)
	}
	return is
}

func (r *RegistryDefault) ReadGRPCServer(ctx context.Context) *grpc.Server {
	s := grpc.NewServer(
		grpc.ChainStreamInterceptor(r.streamInterceptors(ctx)...),
		grpc.ChainUnaryInterceptor(r.unaryInterceptors(ctx)...),
	)

	grpcHealthV1.RegisterHealthServer(s, r.HealthServer())
	acl.RegisterVersionServiceServer(s, r)
	reflection.Register(s)

	for _, h := range r.allHandlers() {
		h.RegisterReadGRPC(s)
	}

	return s
}

func (r *RegistryDefault) WriteGRPCServer(ctx context.Context) *grpc.Server {
	s := grpc.NewServer(
		grpc.ChainStreamInterceptor(r.streamInterceptors(ctx)...),
		grpc.ChainUnaryInterceptor(r.unaryInterceptors(ctx)...),
	)

	grpcHealthV1.RegisterHealthServer(s, r.HealthServer())
	acl.RegisterVersionServiceServer(s, r)
	reflection.Register(s)

	for _, h := range r.allHandlers() {
		h.RegisterWriteGRPC(s)
	}

	return s
}

func (*RegistryDefault) ContextualizeHTTPMiddleware(w http.ResponseWriter, req *http.Request, next http.HandlerFunc) {
	next(w, req)
}

func (*RegistryDefault) ContextualizeGRPCUnaryMiddleware(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	return handler(ctx, req)
}

func (*RegistryDefault) ContextualizeGRPCStreamMiddleware(srv interface{}, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, stream)
}
