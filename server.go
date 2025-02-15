package twtxt

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"

	rice "github.com/GeertJohan/go.rice"
	"github.com/NYTimes/gziphandler"
	"github.com/julienschmidt/httprouter"
	"github.com/robfig/cron"
	log "github.com/sirupsen/logrus"
	"github.com/unrolled/logger"

	"github.com/prologic/twtxt/auth"
	"github.com/prologic/twtxt/password"
	"github.com/prologic/twtxt/session"
)

// Server ...
type Server struct {
	bind      string
	config    *Config
	templates *Templates
	router    *Router
	server    *http.Server

	// Database
	db Store

	// Scheduler
	cron *cron.Cron

	// Auth
	am *auth.Manager

	// Sessions
	sm *session.Manager

	// Passwords
	pm *password.Manager
}

func (s *Server) render(name string, w http.ResponseWriter, ctx *Context) {
	buf, err := s.templates.Exec(name, ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = buf.WriteTo(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// AddRouter ...
func (s *Server) AddRoute(method, path string, handler http.Handler) {
	s.router.Handler(method, path, handler)
}

// AddShutdownHook ...
func (s *Server) AddShutdownHook(f func()) {
	s.server.RegisterOnShutdown(f)
}

// Shutdown ...
func (s *Server) Shutdown(ctx context.Context) error {
	s.cron.Stop()

	if err := s.server.Shutdown(ctx); err != nil {
		log.WithError(err).Error("error shutting down server")
		return err
	}

	if err := s.db.Close(); err != nil {
		log.WithError(err).Error("error closing store")
		return err
	}

	return nil
}

// Run ...
func (s *Server) Run() (err error) {
	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint

		log.Info("Shutting down...")

		// We received an interrupt signal, shut down.
		if err = s.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.WithError(err).Fatal("Error shutting down HTTP server")
		}
		close(idleConnsClosed)
	}()

	if err = s.ListenAndServe(); err != http.ErrServerClosed {
		// Error starting or closing listener:
		log.WithError(err).Fatal("HTTP server ListenAndServe")
	}

	<-idleConnsClosed

	return
}

// ListenAndServe ...
func (s *Server) ListenAndServe() error {
	return s.server.ListenAndServe()
}

// AddCronJob ...
func (s *Server) AddCronJob(spec string, job cron.Job) error {
	return s.cron.AddJob(spec, job)
}

func (s *Server) setupCronJobs() error {
	for spec, factory := range Jobs {
		job := factory(s.config, s.db)
		if err := s.cron.AddJob(spec, job); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) initRoutes() {
	s.router.ServeFilesWithCacheControl(
		"/css/*filepath",
		rice.MustFindBox("static/css").HTTPBox(),
	)

	s.router.ServeFilesWithCacheControl(
		"/img/*filepath",
		rice.MustFindBox("static/img").HTTPBox(),
	)

	s.router.ServeFilesWithCacheControl(
		"/js/*filepath",
		rice.MustFindBox("static/js").HTTPBox(),
	)

	s.router.GET("/favicon.ico", func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		box, err := rice.FindBox("static")
		if err != nil {
			http.Error(w, "404 file not found", http.StatusNotFound)
			return
		}

		buf, err := box.Bytes("favicon.ico")
		if err != nil {
			msg := fmt.Sprintf("error reading favicon: %s", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Cache-Control", "public, max-age=7776000")

		n, err := w.Write(buf)
		if err != nil {
			log.Errorf("error writing response for favicon: %s", err)
		} else if n != len(buf) {
			log.Warnf(
				"not all bytes of favicon response were written: %d/%d",
				n, len(buf),
			)
		}
	})

	s.router.NotFound = http.HandlerFunc(s.NotFoundHandler)

	s.router.GET("/about", s.PageHandler("about"))
	s.router.GET("/help", s.PageHandler("help"))
	s.router.GET("/privacy", s.PageHandler("privacy"))
	s.router.GET("/support", s.PageHandler("support"))

	s.router.GET("/", s.TimelineHandler())
	s.router.HEAD("/", s.TimelineHandler())

	s.router.GET("/discover", s.am.MustAuth(s.DiscoverHandler()))

	s.router.GET("/feeds", s.am.MustAuth(s.FeedsHandler()))
	s.router.POST("/feed", s.am.MustAuth(s.FeedHandler()))

	s.router.POST("/post", s.am.MustAuth(s.PostHandler()))

	s.router.HEAD("/u/:nick", s.TwtxtHandler())
	s.router.GET("/u/:nick", s.TwtxtHandler())

	s.router.GET("/login", s.LoginHandler())
	s.router.POST("/login", s.LoginHandler())

	s.router.GET("/logout", s.LogoutHandler())
	s.router.POST("/logout", s.LogoutHandler())

	s.router.GET("/register", s.RegisterHandler())
	s.router.POST("/register", s.RegisterHandler())

	s.router.GET("/follow", s.am.MustAuth(s.FollowHandler()))
	s.router.POST("/follow", s.am.MustAuth(s.FollowHandler()))

	s.router.GET("/import", s.am.MustAuth(s.ImportHandler()))
	s.router.POST("/import", s.am.MustAuth(s.ImportHandler()))

	s.router.GET("/unfollow", s.am.MustAuth(s.UnfollowHandler()))
	s.router.POST("/unfollow", s.am.MustAuth(s.UnfollowHandler()))

	s.router.GET("/settings", s.am.MustAuth(s.SettingsHandler()))
	s.router.POST("/settings", s.am.MustAuth(s.SettingsHandler()))

	s.router.POST("/delete", s.am.MustAuth(s.DeleteHandler()))
}

// NewServer ...
func NewServer(bind string, options ...Option) (*Server, error) {
	config := NewConfig()

	for _, opt := range options {
		if err := opt(config); err != nil {
			return nil, err
		}
	}

	templates, err := NewTemplates()
	if err != nil {
		log.WithError(err).Error("error loading templates")
		return nil, err
	}

	router := NewRouter()

	am := auth.NewManager(auth.NewOptions("/login", "/register"))

	pm := password.NewManager(nil)

	sm := session.NewManager(
		session.NewOptions(
			config.Name,
			config.CookieSecret,
			strings.HasPrefix(config.BaseURL, "https"),
			config.SessionExpiry,
		),
		session.NewMemoryStore(config.SessionExpiry),
	)

	server := &Server{
		bind:      bind,
		config:    config,
		router:    router,
		templates: templates,

		server: &http.Server{
			Addr: bind,
			Handler: logger.New(logger.Options{
				Prefix:               "twtxt",
				RemoteAddressHeaders: []string{"X-Forwarded-For"},
			}).Handler(
				gziphandler.GzipHandler(
					sm.Handler(router),
				),
			),
		},

		// Schedular
		cron: cron.New(),

		// Auth Manager
		am: am,

		// Session Manager
		sm: sm,

		// Password Manager
		pm: pm,
	}

	db, err := NewStore(server.config.Store)
	if err != nil {
		log.WithError(err).Error("error creating store")
		return nil, err
	}
	server.db = db

	if err := server.setupCronJobs(); err != nil {
		log.WithError(err).Error("error settupt up background jobs")
		return nil, err
	}
	server.cron.Start()
	log.Infof("started background jobs")

	// Log interesting configuration options
	log.Infof("Instance Name: %s", server.config.Name)
	log.Infof("Base URL: %s", server.config.BaseURL)
	log.Infof("Admin User: %s", server.config.AdminUser)
	log.Infof("User Registrationg: %t", server.config.Register)
	log.Infof("Max Tweets per Page: %d", server.config.TweetsPerPage)
	log.Infof("Maximum length of Posts: %d", server.config.MaxTweetLength)

	// Warn about user registration being disabled.
	if !server.config.Register {
		log.Warn("User registrations are disabled as per configuration (no -r/--register)")
	}

	server.initRoutes()

	return server, nil
}
