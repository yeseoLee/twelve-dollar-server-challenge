package main

import (
	"database/sql"
	"net/url"
	"path/filepath"

	"github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("sqlite3_tuned", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		_, err := c.Exec("PRAGMA cache_size=-65536; PRAGMA mmap_size=268435456; PRAGMA temp_store=MEMORY;", nil)
		return err
	}})
}

type store struct {
	db, rdb                                       *sql.DB // db: single writer, rdb: read pool
	health, feed, post, insPost, selPost, insLike *sql.Stmt
	writes                                        chan *writeOp
}

func openStore(path string) (*store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"mode": {"rw"}, "_journal_mode": {"WAL"}, "_synchronous": {"NORMAL"}, "_foreign_keys": {"on"}, "_busy_timeout": {"5000"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3_tuned", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	rdb, err := sql.Open("sqlite3_tuned", uri.String())
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(4)
	rdb.SetMaxIdleConns(4)
	s := &store{db: db, rdb: rdb}
	for _, q := range []struct {
		dst **sql.Stmt
		sql string
	}{
		{&s.health, "SELECT 1"},
		{&s.feed, selectPost + " ORDER BY p.created_at DESC, p.id DESC LIMIT 20"},
		{&s.post, selectPost + " WHERE p.id = ?"},
	} {
		if *q.dst, err = rdb.Prepare(q.sql); err != nil {
			return nil, err
		}
	}
	for _, q := range []struct {
		dst **sql.Stmt
		sql string
	}{
		{&s.insPost, "INSERT INTO posts (user_id, body) VALUES (?, ?) RETURNING id, created_at"},
		{&s.selPost, "SELECT id FROM posts WHERE id = ?"},
		{&s.insLike, "INSERT INTO likes (user_id, post_id) VALUES (?, ?) ON CONFLICT (user_id, post_id) DO NOTHING"},
	} {
		if *q.dst, err = db.Prepare(q.sql); err != nil {
			db.Close()
			return nil, err
		}
	}
	s.startWriter()
	if err := s.Health(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *store) Close() error { s.rdb.Close(); return s.db.Close() }

func (s *store) Health() error {
	var one int
	return s.health.QueryRow().Scan(&one)
}

const selectPost = `SELECT p.id, p.body, p.created_at, u.username,
 (SELECT count(*) FROM likes l WHERE l.post_id = p.id)
 FROM posts p JOIN users u ON u.id = p.user_id`

func (s *store) Feed() ([]post, error) {
	rows, err := s.feed.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts := make([]post, 0, 20)
	for rows.Next() {
		var p post
		if err := rows.Scan(&p.ID, &p.Body, &p.CreatedAt, &p.Author, &p.LikeCount); err != nil {
			return nil, err
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

func (s *store) Post(id int64) (post, error) {
	var p post
	err := s.post.QueryRow(id).Scan(&p.ID, &p.Body, &p.CreatedAt, &p.Author, &p.LikeCount)
	return p, err
}

type writeOp struct {
	like   bool
	actor  identity
	body   postBody
	postID int64
	p      post
	liked  bool // already liked
	err    error
	done   chan struct{}
}

func (s *store) startWriter() {
	s.writes = make(chan *writeOp, 1024)
	go func() {
		batch := make([]*writeOp, 0, 128)
		for op := range s.writes {
			batch = append(batch[:0], op)
		drain:
			for len(batch) < cap(batch) {
				select {
				case o := <-s.writes:
					batch = append(batch, o)
				default:
					break drain
				}
			}
			s.runBatch(batch)
			for _, o := range batch {
				close(o.done)
			}
		}
	}()
}

// runBatch executes all ops in one transaction; callers are answered only after Commit returns.
func (s *store) runBatch(batch []*writeOp) {
	fail := func(err error) {
		for _, o := range batch {
			if o.err == nil {
				o.err = err
			}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		fail(err)
		return
	}
	defer tx.Rollback()
	insPost, selPost, insLike := tx.Stmt(s.insPost), tx.Stmt(s.selPost), tx.Stmt(s.insLike)
	for _, o := range batch {
		if o.like {
			var id int64
			if err := selPost.QueryRow(o.postID).Scan(&id); err != nil {
				o.err = err
				continue
			}
			res, err := insLike.Exec(o.actor.userID, o.postID)
			if err != nil {
				fail(err)
				return
			}
			n, err := res.RowsAffected()
			if err != nil {
				fail(err)
				return
			}
			o.liked = n == 0
			continue
		}
		o.p = post{Body: string(o.body), Author: o.actor.username}
		if err := insPost.QueryRow(o.actor.userID, string(o.body)).Scan(&o.p.ID, &o.p.CreatedAt); err != nil {
			fail(err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(err)
	}
}

func (s *store) submit(o *writeOp) error {
	o.done = make(chan struct{})
	s.writes <- o
	<-o.done
	return o.err
}

func (s *store) CreatePost(actor identity, body postBody) (post, error) {
	o := &writeOp{actor: actor, body: body}
	err := s.submit(o)
	return o.p, err
}

func (s *store) Like(actor identity, postID int64) (bool, error) {
	o := &writeOp{like: true, actor: actor, postID: postID}
	err := s.submit(o)
	return o.liked, err
}
