// vim: set ts=4 sw=4 noet:
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/gin-gonic/gin"
)

const timeFormat = "02/Jan/2006:15:04:05 -0700"

type RequestData struct {
	CitizenID string `json:"citizenId" binding:"required"`
	Day       string `json:"day"`
	Year      string `json:"year"`
	Fn        string `json:"fn"`
	Sn1       string `json:"sn1"`
	Sn2       string `json:"sn2"`
	PostCode  string `json:"postCode"`
	Colele    string `json:"colele"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 && os.Args[1] == "analizar" {
		os.Exit(analyze(os.Args[2:], os.Stdout))
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// The HTTP server starts right away so /health can report 503 while a
	// large census is still being imported.
	var db atomic.Pointer[sql.DB]
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           newRouter(cfg, &db),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("Listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	go func() {
		d, err := prepareDatabase(cfg)
		if err != nil {
			log.Fatalf("Error preparing database: %v", err)
		}
		db.Store(d)
		log.Printf("Database ready")
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}
	if d := db.Load(); d != nil {
		d.Close()
	}
}

func newRouter(cfg Config, db *atomic.Pointer[sql.DB]) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(customLogger(cfg.Location), gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		if db.Load() != nil {
			c.Status(http.StatusOK)
		} else {
			c.Status(http.StatusServiceUnavailable)
		}
	})

	// Which part of the identity document is indexed, so that clients ask the
	// citizen for just that part and the whole document never leaves them.
	r.GET("/formato", TokenAuthMiddleware(cfg.Token), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"documentChars": cfg.DocumentChars,
			"firstChars":    cfg.FirstChars,
			"addLetter":     cfg.FirstChars && cfg.FirstCharsAddLetter,
		})
	})

	// Errors are answered with 200 and an errorMessage so that chatbot
	// platforms such as MessageBird can branch on the body.
	r.POST("/consulta", TokenAuthMiddleware(cfg.Token), func(c *gin.Context) {
		d := db.Load()
		if d == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"errorMessage": "service loading"})
			return
		}

		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var req RequestData
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusOK, gin.H{"errorMessage": "invalid request: " + err.Error()})
			return
		}

		// Fields that are not indexed are stored empty, so filtering on them
		// would never match: ignore them instead.
		key := CitizenKey{
			CitizenID: cfg.documentKey(req.CitizenID),
			Colele:    strings.ToUpper(strings.TrimSpace(req.Colele)),
		}
		if cfg.Day {
			key.Day = dayKey(req.Day)
		}
		if cfg.Year {
			key.Year = yearKey(req.Year)
		}
		if cfg.Fn && req.Fn != "" {
			key.Fn = cfg.nameKey(req.Fn)
		}
		if cfg.Sn1 && req.Sn1 != "" {
			key.Sn1 = cfg.nameKey(req.Sn1)
		}
		if cfg.Sn2 && req.Sn2 != "" {
			key.Sn2 = cfg.nameKey(req.Sn2)
		}
		if cfg.PostCode {
			key.PostCode = strings.TrimSpace(req.PostCode)
		}
		if key.CitizenID == "" {
			c.JSON(http.StatusOK, gin.H{"errorMessage": "invalid request: citizenId"})
			return
		}

		info, err := lookupCitizen(d, key)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"errorMessage": err.Error()})
			return
		}
		c.JSON(http.StatusOK, info)
	})

	return r
}

func customLogger(location *time.Location) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if c.Request.URL.Path == "/health" {
			return
		}
		log.Printf("%s - - [%s] \"%s %s %s\" %d %d \"%s\" \"%s\"",
			c.ClientIP(),
			time.Now().In(location).Format(timeFormat),
			c.Request.Method,
			c.Request.URL.Path,
			c.Request.Proto,
			c.Writer.Status(),
			c.Writer.Size(),
			c.Request.Referer(),
			c.Request.UserAgent(),
		)
	}
}

func TokenAuthMiddleware(envToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimPrefix(c.Request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(envToken)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Unauthorized"})
			return
		}
		c.Next()
	}
}

// healthcheck lets the distroless image, which has no curl, probe itself.
func healthcheck() int {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + envString("PORT", "8080") + "/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
