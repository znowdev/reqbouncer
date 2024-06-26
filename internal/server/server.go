package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lxzan/gws"
	slogecho "github.com/samber/slog-echo"
	"github.com/znowdev/reqbouncer/internal/client/auth"
	"github.com/znowdev/reqbouncer/internal/wire"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var connectedClients atomic.Int32

type Config struct {
	GithubClientid     string
	GithubUserProvider auth.GithubUserProvider
	CiTestToken        string
	Port               string
	Debug              bool
}

func Start(logger *slog.Logger, cfg Config) error {
	logger = logger.With("component", "server")
	e := echo.New()
	e.Use(subdomainMw)
	e.Use(slogecho.NewWithConfig(logger, slogecho.Config{
		DefaultLevel:       slog.LevelInfo,
		ClientErrorLevel:   slog.LevelWarn,
		ServerErrorLevel:   slog.LevelError,
		WithUserAgent:      false,
		WithRequestID:      false,
		WithRequestBody:    false,
		WithRequestHeader:  false,
		WithResponseBody:   false,
		WithResponseHeader: false,
		WithSpanID:         false,
		WithTraceID:        false,
		Filters: []slogecho.Filter{
			slogecho.IgnorePath("/healthz"),
			IgnoreUserAgent("Mozilla/5.0 (compatible; CensysInspect/1.1; +https://about.censys.io/)"),
		},
	}))
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())
	e.Use(middleware.Gzip())
	e.Use(middleware.Secure())
	e.Use(middleware.RequestID())
	e.Use(middleware.BodyLimit("1M"))
	e.Use(middleware.Decompress())
	pubSub := gochannel.NewGoChannel(
		gochannel.Config{},
		watermill.NewStdLogger(false, false),
	)

	cm := &clientMap{clients: make(map[string]struct{})}

	upgrader := gws.NewUpgrader(&Handler{cm, pubSub}, &gws.ServerOption{
		WriteBufferSize:     0,
		PermessageDeflate:   gws.PermessageDeflate{Enabled: true}, // Enable compression
		ParallelEnabled:     true,                                 // Parallel message processing
		ParallelGolimit:     0,
		ReadMaxPayloadSize:  0,
		ReadBufferSize:      0,
		WriteMaxPayloadSize: 0,
		CheckUtf8Enabled:    false,
		Logger:              nil,
		Recovery:            nil,
		TlsConfig:           nil,
		HandshakeTimeout:    time.Second * 5,
		SubProtocols:        nil,
		ResponseHeader:      nil,
		Authorize:           nil,
		NewSession:          nil,
	})
	srv := &server{upgrader, cfg.GithubClientid, pubSub, cm}

	authMw := newAuthMiddleware(cfg.CiTestToken, cfg.GithubUserProvider)

	//myRouter.HandleFunc("/debug/pprof/", pprof.Index)
	//myRouter.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	//myRouter.HandleFunc("/debug/pprof/profile", pprof.Profile)
	//myRouter.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	//myRouter.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	//myRouter.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	//myRouter.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	//myRouter.Handle("/debug/pprof/block", pprof.Handler("block"))

	if cfg.Debug {
		e.GET("/debug/pprof", echo.WrapHandler(http.DefaultServeMux))
		e.Any("/debug/pprof/*", echo.WrapHandler(http.DefaultServeMux))
	}

	e.GET("/_config", srv.configHandler)
	e.GET("/_health", srv.healthHandler)
	e.GET("/_websocket", srv.handleSockets, authMw, checkSubDomain(cm))
	e.RouteNotFound("/*", srv.forwardRequest, ensureSubdomainHasListeners(cm))

	err := e.Start(":" + cfg.Port)
	if err != nil {
		return err
	}

	return nil

}
func IgnoreUserAgent(urls ...string) slogecho.Filter {
	return func(c echo.Context) bool {
		for _, url := range urls {
			if c.Request().Header.Get("user-agent") == url {
				return false
			}
		}

		return true
	}
}
func subdomainMw(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		host := c.Request().Host
		subdomain := strings.Split(host, ".")[0]
		c.Set("subdomain", subdomain)
		return next(c)
	}
}

func checkSubDomain(cm *clientMap) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			subdomain := c.Get("subdomain").(string)
			if cm.HasClient(subdomain) {
				slog.Error("client already connected", slog.Any("subdomain", subdomain))
				return c.JSON(http.StatusConflict, echo.Map{"error": "client already connected"})
			}
			return next(c)
		}
	}
}

func ensureSubdomainHasListeners(cm *clientMap) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			subdomain := c.Get("subdomain").(string)
			if !cm.HasClient(subdomain) {
				slog.Error("no clients connected for subdomain", slog.Any("subdomain", subdomain))
				return c.JSON(http.StatusConflict, echo.Map{"error": fmt.Sprintf("no clients connected for host'%s'", c.Request().Host)})
			}
			return next(c)
		}
	}
}

const (
	PingInterval = 5 * time.Second
	PingWait     = 10 * time.Second
)

const (
	CloseNormalClosure = 1000
)

type Handler struct {
	clientMap *clientMap
	pubSub    *gochannel.GoChannel
}

var clientConnMux = sync.Mutex{}
var clientConns = make(map[*gws.Conn]string)

func (c *Handler) OnOpen(socket *gws.Conn) {
	slog.Debug("socket opened")
	clientConnMux.Lock()
	defer clientConnMux.Unlock()
	clientConns[socket] = socket.RemoteAddr().String()
	_ = socket.SetDeadline(time.Now().Add(PingInterval + PingWait))
	v, ok := socket.Session().Load("subdomain")
	if ok {

		if c.clientMap.HasClient(v.(string)) {
			slog.Info("client already connected for subdomain", slog.Any("subdomain", v))
			socket.WriteClose(CloseNormalClosure, []byte("client already connected"))
			defer socket.NetConn().Close()
			return
		}
		slog.Info("socket connected to subdomain", slog.Any("subdomain", v))
		c.clientMap.AddClient(v.(string))

		ctx := context.Background()
		var clientMessages <-chan *message.Message
		clientMessages, err := c.pubSub.Subscribe(ctx, v.(string))
		if err != nil {
			socket.WriteClose(CloseNormalClosure, []byte("could not subscribe to client topic"))
		}

		go func() {
			for {
				select {
				case msg := <-clientMessages:
					slog.Debug("sending client message", slog.Any("message_id", msg.UUID))

					wireMsg := wire.WireMessage{
						ID:      msg.UUID,
						Payload: msg.Payload,
					}

					bytes, err := wireMsg.Serialize()
					if err != nil {
						slog.Error("failed to serialize message", slog.Any("error", err))
						continue
					}

					err = socket.WriteMessage(gws.OpcodeBinary, bytes)
					if err != nil {
						slog.Error("failed to write message", slog.Any("error", err))
						continue
					}
					msg.Ack()

				}
			}
		}()
	}

}

func (c *Handler) OnClose(socket *gws.Conn, err error) {
	slog.Debug("connection closed", slog.Any("error", err))
	v, ok := socket.Session().Load("subdomain")
	if ok {
		slog.Info("socket closed", slog.Any("subdomain", v))
		c.clientMap.RemoveClient(v.(string))
	}
}

func (c *Handler) OnPing(socket *gws.Conn, payload []byte) {
	//slog.Debug("received ping")
	_ = socket.SetDeadline(time.Now().Add(PingInterval + PingWait))
	_ = socket.WritePong(nil)
}

func (c *Handler) OnPong(socket *gws.Conn, payload []byte) {
	//slog.Debug("received pong")
	_ = socket.SetDeadline(time.Now().Add(PingInterval + PingWait))
}

func (c *Handler) OnMessage(socket *gws.Conn, wsmsg *gws.Message) {
	defer wsmsg.Close()

	if wsmsg.Opcode != gws.OpcodeBinary {
		slog.Error("received non-binary message")
		return
	}

	slog.Debug("received binary message", slog.Any("content", string(wsmsg.Bytes())))
	var wireMsg wire.WireMessage
	if err := wireMsg.Deserialize(wsmsg.Bytes()); err != nil {
		slog.Error("failed to deserialize message", slog.Any("error", err))
		return
	}
	msg := message.NewMessage(wireMsg.ID, wireMsg.Payload)
	slog.Debug("publishing message", slog.Any("message_id", msg.UUID))
	err := c.pubSub.Publish(wireMsg.ID, msg)
	if err != nil {
		slog.Error("failed to publish message", slog.Any("error", err))
		return
	}
	return

}

type server struct {
	*gws.Upgrader
	githubClientid string
	pubSub         *gochannel.GoChannel
	clientMap      *clientMap
}

func (s *server) healthHandler(c echo.Context) error {
	return c.JSON(200, echo.Map{"status": "ok"})
}

func (s *server) configHandler(c echo.Context) error {
	return c.JSON(200, echo.Map{"github_client_id": s.githubClientid})
}

func (s *server) forwardRequest(c echo.Context) error {
	requestId := uuid.NewString()

	buf := new(bytes.Buffer)
	err := c.Request().Write(buf)
	if err != nil {
		return err
	}

	msg := message.NewMessage(requestId, buf.Bytes())

	slog.Debug("publishing message", slog.Any("message_id", msg.UUID))
	err = s.pubSub.Publish(c.Get("subdomain").(string), msg)
	if err != nil {
		return err
	}

	msgs, err := s.pubSub.Subscribe(c.Request().Context(), requestId)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()

	for {
		select {
		case <-c.Request().Context().Done():
			return c.Request().Context().Err()
		case msg := <-msgs:
			slog.Debug("received response", slog.Any("message_id", msg.UUID))

			hj, ok := c.Response().Writer.(http.Hijacker)
			if !ok {
				return errors.New("webserver does not support hijacking")
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				return err
			}
			defer conn.Close()
			fmt.Fprintf(bufrw, "%s", msg.Payload)
			bufrw.Flush()
			conn.Close()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

}

func (ws *server) handleSockets(c echo.Context) error {
	socket, err := ws.Upgrade(c.Response(), c.Request())
	if err != nil {
		return err
	}

	socket.Session().Store("subdomain", c.Get("subdomain"))

	socket.ReadLoop()

	return nil
}
