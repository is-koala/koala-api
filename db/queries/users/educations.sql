-- name: LockEducationWrites :exec
-- LockEducationWrites serializes the education writes of one user until
-- the transaction ends, so the per-user cap counts and then inserts
-- without a concurrent create passing it too (ADR-0036 Karar 6). The
-- two-key form with its own class keeps it apart from the other advisory
-- locks.
SELECT pg_advisory_xact_lock(sqlc.arg(lock_class)::integer, sqlc.arg(lock_key)::integer);

-- name: CountEducations :one
-- CountEducations counts the user's active education records.
SELECT count(*)
FROM active_educations
WHERE user_id = $1;

-- name: GetUniversity :one
-- GetUniversity finds a university that may be chosen: not retired, or
-- retired but kept (allow_retired: the record already names it).
SELECT id
FROM universities
WHERE id = sqlc.arg(id) AND (retired_at IS NULL OR sqlc.arg(allow_retired)::boolean);

-- name: GetFacultyOf :one
-- GetFacultyOf finds a faculty of the university that may be chosen
-- (allow_retired as GetUniversity).
SELECT id
FROM faculties
WHERE id = sqlc.arg(id) AND university_id = sqlc.arg(university_id)
  AND (retired_at IS NULL OR sqlc.arg(allow_retired)::boolean);

-- name: GetDepartmentOf :one
-- GetDepartmentOf finds a department of the faculty that may be chosen
-- (allow_retired as GetUniversity).
SELECT id
FROM departments
WHERE id = sqlc.arg(id) AND faculty_id = sqlc.arg(faculty_id)
  AND (retired_at IS NULL OR sqlc.arg(allow_retired)::boolean);

-- name: InsertEducation :exec
-- InsertEducation adds an education record. The caller holds the user's
-- active row (softdelete.LockActiveParent) and the education write lock.
INSERT INTO active_educations (id, user_id, university_id, faculty_id, department_id, degree, start_year, end_year,
    created_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(university_id), sqlc.narg(faculty_id), sqlc.narg(department_id),
    sqlc.arg(degree), sqlc.arg(start_year), sqlc.narg(end_year), sqlc.arg(created_at));

-- name: ReplaceEducation :execrows
-- ReplaceEducation rewrites one of the user's education records; another
-- user's and a deleted one change no row.
UPDATE active_educations
SET university_id = sqlc.arg(university_id),
    faculty_id = sqlc.narg(faculty_id),
    department_id = sqlc.narg(department_id),
    degree = sqlc.arg(degree),
    start_year = sqlc.arg(start_year),
    end_year = sqlc.narg(end_year)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: LockEducation :one
-- LockEducation locks one of the user's active education records; another
-- user's is not found. The references it names come back: a retired one
-- the record keeps may stay (ADR-0036).
SELECT id, university_id, faculty_id, department_id
FROM active_educations
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
FOR UPDATE;

-- name: GetEducation :one
-- GetEducation is one of the user's education records with the names of
-- its reference rows.
SELECT e.id, e.degree, e.start_year, e.end_year, e.created_at,
       un.id AS university_id, un.name AS university_name,
       fa.id AS faculty_id, fa.name AS faculty_name,
       de.id AS department_id, de.name AS department_name
FROM active_educations e
JOIN universities un ON un.id = e.university_id
LEFT JOIN faculties fa ON fa.id = e.faculty_id
LEFT JOIN departments de ON de.id = e.department_id
WHERE e.id = sqlc.arg(id) AND e.user_id = sqlc.arg(user_id);

-- name: ListEducations :many
-- ListEducations is the user's education records, the latest end first:
-- an ongoing one (no end year) leads, then by end year, start year and
-- id, all descending. The list is short (EducationsPerUser), so the page
-- is cut in the service.
SELECT e.id, e.degree, e.start_year, e.end_year, e.created_at,
       un.id AS university_id, un.name AS university_name,
       fa.id AS faculty_id, fa.name AS faculty_name,
       de.id AS department_id, de.name AS department_name
FROM active_educations e
JOIN universities un ON un.id = e.university_id
LEFT JOIN faculties fa ON fa.id = e.faculty_id
LEFT JOIN departments de ON de.id = e.department_id
WHERE e.user_id = sqlc.arg(user_id)
ORDER BY coalesce(e.end_year, 9999) DESC, e.start_year DESC, e.id DESC;
