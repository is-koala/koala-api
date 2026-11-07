//go:build integration

package softdeletetest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/softdelete/softdeletetest"
)

// The generated JOIN test is green on the example resources: the comment
// endpoints reach the author by LEFT JOIN active_example_owners, so a
// comment whose author is soft-deleted keeps its row and loses only its
// author field. This is the positive side of the detach rule, the one the
// real contract has no operation for yet (every real detach parent is
// either the caller or a device whose session is dead without it).
func TestJoinTestOnExampleResources(t *testing.T) {
	t.Parallel()

	spec, err := openapi3.NewLoader().LoadFromFile("testdata/openapi-join.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app := dbtest.New(t).Open(t, db.RoleApp)

	softdeletetest.JoinTest{
		Spec:    spec,
		Handler: commentsHandler(app),
		App:     app,
		Catalog: exampleOnlyCatalog(t),
		Cases: map[string]softdeletetest.DetachCase{
			"example_comments.author_id": {
				Fixture:    insertCommentWithAuthor(app),
				Outcome:    softdeletetest.Keeps,
				NullFields: []string{"author"},
			},
		},
	}.Run(t)
}

// exampleOnlyCatalog is the catalog of the example tables alone, so the
// test declares one detach edge instead of the real contract's.
func exampleOnlyCatalog(t *testing.T) *softdelete.Catalog {
	t.Helper()
	data, err := os.ReadFile("../../dbtest/testdata/softdelete.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := softdelete.ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// insertCommentWithAuthor creates one active owner with one comment.
func insertCommentWithAuthor(app *db.DB) func(ctx context.Context) (softdeletetest.Row, error) {
	return func(ctx context.Context) (softdeletetest.Row, error) {
		owner, comment := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		err := app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
			if _, err := q.Exec(ctx, "INSERT INTO active_example_owners (id, name) VALUES ($1, 'o')", owner); err != nil {
				return fmt.Errorf("insert owner: %w", err)
			}
			if err := softdelete.LockActiveParent(ctx, q, "example_owners", owner); err != nil {
				return fmt.Errorf("lock owner: %w", err)
			}
			_, err := q.Exec(ctx, "INSERT INTO active_example_comments (id, author_id, body) VALUES ($1, $2, 'hi')", comment, owner)
			if err != nil {
				return fmt.Errorf("insert comment: %w", err)
			}
			return nil
		})
		if err != nil {
			return softdeletetest.Row{}, err
		}
		return softdeletetest.Row{Child: comment, Parent: owner}, nil
	}
}

// comment is one example comment with its author, the way a domain's
// projection would build it.
type comment struct {
	ID     uuid.UUID `json:"id"`
	Body   string    `json:"body"`
	Author *author   `json:"author"`
}

type author struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// rowScanner is the Scan that both pgx.Row and pgx.CollectableRow offer.
type rowScanner interface{ Scan(dest ...any) error }

// scanComment reads one comment with its author; the author of a
// soft-deleted owner comes back NULL and the comment keeps its row.
func scanComment(row rowScanner) (comment, error) {
	var c comment
	var id uuid.NullUUID
	var name *string
	if err := row.Scan(&c.ID, &c.Body, &id, &name); err != nil {
		return comment{}, err
	}
	if id.Valid && name != nil {
		c.Author = &author{ID: id.UUID, Name: *name}
	}
	return c, nil
}

// commentsHandler serves testdata/openapi-join.yaml the way a domain's
// http.go would: the child is read from its own active view and the author
// is reached by LEFT JOIN, so a deleted author is NULL and the comment
// stays.
func commentsHandler(app *db.DB) http.Handler {
	logger := slog.New(slog.DiscardHandler)
	const columns = `SELECT c.id, c.body, o.id, o.name
		FROM active_example_comments c
		LEFT JOIN active_example_owners o ON o.id = c.author_id`
	writeJSON := func(w http.ResponseWriter, r *http.Request, v any) {
		body, err := json.Marshal(v)
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /example-comments", func(w http.ResponseWriter, r *http.Request) {
		rows, err := app.Pool().Query(r.Context(), columns+" ORDER BY c.id")
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (comment, error) {
			return scanComment(row)
		})
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		writeJSON(w, r, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /example-comments/{commentId}", func(w http.ResponseWriter, r *http.Request) {
		notFound := apperr.New(apperr.KindNotFound, "EXAMPLE_COMMENT_NOT_FOUND")
		id, err := uuid.Parse(r.PathValue("commentId"))
		if err != nil {
			httpx.WriteError(w, r, logger, notFound)
			return
		}
		item, err := scanComment(app.Pool().QueryRow(r.Context(), columns+" WHERE c.id = $1", id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			httpx.WriteError(w, r, logger, notFound)
		case err != nil:
			httpx.WriteError(w, r, logger, err)
		default:
			writeJSON(w, r, item)
		}
	})
	return mux
}
