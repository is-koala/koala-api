//go:build integration

package softdeletetest_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete/softdeletetest"
)

// The generated read test is green on the example resources: the handler
// reads through the active views, so a soft-deleted owner or project is a
// 404 like a missing one and drops out of the list.
func TestReadTestOnExampleResources(t *testing.T) {
	t.Parallel()

	spec, err := openapi3.NewLoader().LoadFromFile("testdata/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app := dbtest.New(t).Open(t, db.RoleApp)

	softdeletetest.ReadTest{
		Spec:    spec,
		Handler: exampleHandler(app),
		App:     app,
		Catalog: dbtest.SoftDeleteCatalog(t),
		Fixtures: map[string]softdeletetest.Fixture{
			"example_owners": insertOwner,
			"example_projects": func(ctx context.Context, q db.Querier) (uuid.UUID, error) {
				owner, err := insertOwner(ctx, q)
				if err != nil {
					return uuid.Nil, err
				}
				id := uuid.Must(uuid.NewV7())
				_, err = q.Exec(ctx, "INSERT INTO active_example_projects (id, owner_id, name) VALUES ($1, $2, 'p')", id, owner)
				return id, err
			},
		},
	}.Run(t)
}

func insertOwner(ctx context.Context, q db.Querier) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	_, err := q.Exec(ctx, "INSERT INTO active_example_owners (id, name) VALUES ($1, 'o')", id)
	return id, err
}

type item struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// exampleHandler serves testdata/openapi.yaml the way a domain's http.go
// would: reads go through active_<t> views and a missing row is the
// resource's own 404 problem.
func exampleHandler(app *db.DB) http.Handler {
	logger := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, r *http.Request, v any) {
		body, err := json.Marshal(v)
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	detail := func(view string, notFound apperr.Code, param string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, err := uuid.Parse(r.PathValue(param))
			if err != nil {
				httpx.WriteError(w, r, logger, apperr.New(apperr.KindNotFound, notFound))
				return
			}
			var it item
			err = app.Pool().QueryRow(r.Context(), "SELECT id, name FROM "+view+" WHERE id = $1", id).Scan(&it.ID, &it.Name)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				httpx.WriteError(w, r, logger, apperr.New(apperr.KindNotFound, notFound))
			case err != nil:
				httpx.WriteError(w, r, logger, err)
			default:
				writeJSON(w, r, it)
			}
		}
	}
	mux.HandleFunc("GET /example-owners", func(w http.ResponseWriter, r *http.Request) {
		rows, err := app.Pool().Query(r.Context(), "SELECT id, name FROM active_example_owners ORDER BY id")
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		writeJSON(w, r, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /example-owners/{ownerId}", detail("active_example_owners", "EXAMPLE_OWNER_NOT_FOUND", "ownerId"))
	mux.HandleFunc("GET /example-projects/{projectId}", detail("active_example_projects", "EXAMPLE_PROJECT_NOT_FOUND", "projectId"))
	return mux
}
