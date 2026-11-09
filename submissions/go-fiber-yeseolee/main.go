package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"
)

type post struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Author    string `json:"author"`
	LikeCount int64  `json:"like_count"`
}
type postResponse struct {
	Post post `json:"post"`
}
type feedResponse struct {
	Posts []post `json:"posts"`
}
type errorResponse struct {
	Error string `json:"error"`
}
type likeResponse struct {
	Liked        bool  `json:"liked"`
	AlreadyLiked bool  `json:"already_liked"`
	PostID       int64 `json:"post_id"`
}
type healthResponse struct {
	Status  string `json:"status"`
	DB      string `json:"db"`
	UptimeS int64  `json:"uptime_s"`
}
type degradedResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
	Error  string `json:"error"`
}
type postBody string

func parsePostID(raw string) (int64, error) {
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid post id")
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid post id")
	}
	return id, nil
}

type postReq struct {
	Body any `json:"body"`
}

func parsePostBody(raw []byte) (postBody, error) {
	if !json.Valid(raw) {
		return "", errors.New("malformed JSON body")
	}
	var req postReq
	_ = json.Unmarshal(raw, &req) // non-object JSON leaves Body nil -> "body is required"
	body, ok := req.Body.(string)
	if !ok {
		return "", errors.New("body is required")
	}
	body = strings.TrimFunc(body, func(r rune) bool {
		return r == '\uFEFF' || r == '\u0009' || r == '\u000A' || r == '\u000B' || r == '\u000C' || r == '\u000D' || r == '\u0020' || r == '\u00A0' || r == '\u1680' || (r >= '\u2000' && r <= '\u200A') || r == '\u2028' || r == '\u2029' || r == '\u202F' || r == '\u205F' || r == '\u3000'
	})
	if body == "" {
		return "", errors.New("body is required")
	}
	if utf8.RuneCountInString(body) > 500 {
		return "", errors.New("body must be at most 500 characters")
	}
	return postBody(body), nil
}

func sendJSON(c *fiber.Ctx, status int, b []byte) error {
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
	return c.Status(status).Send(b)
}

func newApp(s *store, secret []byte, started time.Time) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true, IdleTimeout: 120 * time.Second, JSONEncoder: json.Marshal, ErrorHandler: func(c *fiber.Ctx, err error) error {
		if errors.Is(err, sql.ErrNoRows) {
			return c.Status(404).JSON(errorResponse{"post not found"})
		}
		var httpErr *fiber.Error
		if errors.As(err, &httpErr) && httpErr.Code < 500 {
			return c.Status(httpErr.Code).JSON(errorResponse{httpErr.Message})
		}
		log.Printf("request failed: %v", err)
		return c.Status(500).JSON(errorResponse{"internal server error"})
	}})
	app.Get("/health", func(c *fiber.Ctx) error {
		if err := s.Health(); err != nil {
			return c.Status(503).JSON(degradedResponse{"degraded", "unreachable", err.Error()})
		}
		buf := append(make([]byte, 0, 64), `{"status":"ok","db":"ok","uptime_s":`...)
		buf = strconv.AppendInt(buf, int64(time.Since(started)/time.Second), 10)
		return sendJSON(c, 200, append(buf, '}'))
	})
	app.Get("/feed", func(c *fiber.Ctx) error {
		posts, err := s.Feed()
		if err != nil {
			return err
		}
		buf := append(make([]byte, 0, 4096), `{"posts":[`...)
		for i, p := range posts {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendPost(buf, p)
		}
		return sendJSON(c, 200, append(buf, ']', '}'))
	})
	app.Get("/posts/:id", func(c *fiber.Ctx) error {
		id, err := parsePostID(c.Params("id"))
		if err != nil {
			return fiber.NewError(400, err.Error())
		}
		p, err := s.Post(id)
		if err != nil {
			return err
		}
		return sendJSON(c, 200, append(appendPost(append(make([]byte, 0, 256), `{"post":`...), p), '}'))
	})
	app.Post("/posts", func(c *fiber.Ctx) error {
		actor, err := authenticate(c.Get("Authorization"), secret)
		if err != nil {
			return fiber.NewError(401, err.Error())
		}
		body, err := parsePostBody(c.Body())
		if err != nil {
			return fiber.NewError(400, err.Error())
		}
		p, err := s.CreatePost(actor, body)
		if err != nil {
			return err
		}
		return sendJSON(c, 201, append(appendPost(append(make([]byte, 0, 256), `{"post":`...), p), '}'))
	})
	app.Post("/posts/:id/like", func(c *fiber.Ctx) error {
		actor, err := authenticate(c.Get("Authorization"), secret)
		if err != nil {
			return fiber.NewError(401, err.Error())
		}
		id, err := parsePostID(c.Params("id"))
		if err != nil {
			return fiber.NewError(400, err.Error())
		}
		already, err := s.Like(actor, id)
		if err != nil {
			return err
		}
		status := 201
		if already {
			status = 200
		}
		buf := append(make([]byte, 0, 64), `{"liked":true,"already_liked":`...)
		buf = strconv.AppendBool(buf, already)
		buf = append(buf, `,"post_id":`...)
		buf = strconv.AppendInt(buf, id, 10)
		return sendJSON(c, status, append(buf, '}'))
	})
	app.Use(func(c *fiber.Ctx) error { return c.Status(404).JSON(errorResponse{"not found"}) })
	return app
}

func main() {
	debug.SetGCPercent(400)
	debug.SetMemoryLimit(1536 << 20)
	started := time.Now()
	path, secret := os.Getenv("SQLITE_PATH"), os.Getenv("JWT_SECRET")
	if path == "" || secret == "" {
		log.Fatal("SQLITE_PATH and JWT_SECRET are required")
	}
	s, err := openStore(path)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	host, port := os.Getenv("HOST"), os.Getenv("PORT")
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "3000"
	}
	log.Fatal(newApp(s, []byte(secret), started).Listen(net.JoinHostPort(host, port)))
}
