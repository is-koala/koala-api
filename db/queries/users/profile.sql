-- name: GetProfile :one
-- GetProfile is the caller's own profile (GET /v1/me/profile). The phone
-- is the verified number; a number being verified is not on the row yet
-- (USR-014).
SELECT id, email, phone, status, first_name, last_name, birth_date, gender, job_title, about, qualifications,
       interests, profile_picture_id, cv_id, employer, email_verified, phone_verified, identity_verified, created_at,
       address_id
FROM active_users
WHERE id = $1;

-- name: LockProfile :one
-- LockProfile locks the caller's active row FOR UPDATE for a profile
-- patch: the identity freeze (USR-024) is read under the lock that its
-- write holds, and a verification the patch starts finds the row locked
-- by its own transaction, the order every start takes (ADR-0033 Karar 5).
SELECT id, email, phone, status, first_name, last_name, birth_date, gender, job_title, about, qualifications,
       interests, profile_picture_id, cv_id, employer, email_verified, phone_verified, identity_verified, created_at,
       address_id
FROM active_users
WHERE id = $1
FOR UPDATE;

-- name: UpdateProfile :exec
-- UpdateProfile writes the profile fields of a patch; the caller holds
-- the row lock (LockProfile). The e-mail and the phone are not among
-- them: only a confirmed verification writes those (USR-014). A media
-- link it writes was bound in the same transaction (media.Bind).
UPDATE active_users
SET first_name = sqlc.arg(first_name)::text,
    last_name = sqlc.arg(last_name)::text,
    birth_date = sqlc.narg(birth_date)::date,
    gender = sqlc.narg(gender)::text,
    job_title = sqlc.narg(job_title)::text,
    about = sqlc.narg(about)::text,
    qualifications = sqlc.narg(qualifications)::text,
    interests = sqlc.narg(interests)::text,
    profile_picture_id = sqlc.narg(profile_picture_id)::uuid,
    cv_id = sqlc.narg(cv_id)::uuid
WHERE id = sqlc.arg(id);
