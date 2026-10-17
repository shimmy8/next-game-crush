package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"next-game-crush/internal/store"
)

type SQLRepository struct{ store *store.Store }

func NewSQLRepository(database *store.Store) *SQLRepository { return &SQLRepository{store: database} }

func tokenHash(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }

func (r *SQLRepository) SaveLoginState(ctx context.Context, state LoginState) error {
	_, err := r.store.Pool.Exec(ctx, `INSERT INTO openid_login_states (token_hash, return_path, expires_at) VALUES ($1,$2,$3)`, tokenHash(state.Token), state.ReturnPath, state.ExpiresAt)
	return err
}

func (r *SQLRepository) GetLoginState(ctx context.Context, token string, now time.Time) (LoginState, error) {
	var state LoginState
	err := r.store.Pool.QueryRow(ctx, `SELECT return_path, expires_at FROM openid_login_states WHERE token_hash=$1 AND expires_at>$2`, tokenHash(token), now).Scan(&state.ReturnPath, &state.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginState{}, ErrInvalidState
	}
	state.Token = token
	return state, err
}

func (r *SQLRepository) FinishLogin(ctx context.Context, stateToken, nonce string, nonceExpiry time.Time, steamID string, session Session, oldSessionID *uuid.UUID) (LoginState, Session, error) {
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return LoginState{}, Session{}, err
	}
	defer tx.Rollback(ctx)
	var state LoginState
	state.Token = stateToken
	err = tx.QueryRow(ctx, `DELETE FROM openid_login_states WHERE token_hash=$1 AND expires_at>now() RETURNING return_path, expires_at`, tokenHash(stateToken)).Scan(&state.ReturnPath, &state.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginState{}, Session{}, ErrInvalidState
	}
	if err != nil {
		return LoginState{}, Session{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO openid_response_nonces (nonce_hash, expires_at) VALUES ($1,$2)`, tokenHash(nonce), nonceExpiry)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return LoginState{}, Session{}, ErrReplay
		}
		return LoginState{}, Session{}, err
	}
	var userID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT user_id FROM steam_accounts WHERE steam_id=$1`, steamID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		userID = uuid.New()
		if _, err = tx.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
			return LoginState{}, Session{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO steam_accounts (user_id, steam_id, source, availability) VALUES ($1,$2,'steam','unknown')`, userID, steamID); err != nil {
			return LoginState{}, Session{}, err
		}
	} else if err != nil {
		return LoginState{}, Session{}, err
	}
	session.UserID = userID
	session.SteamID = steamID
	session.ReturnPath = state.ReturnPath
	if oldSessionID != nil {
		result, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, *oldSessionID)
		if err != nil {
			return LoginState{}, Session{}, err
		}
		if result.RowsAffected() != 1 {
			return LoginState{}, Session{}, ErrUnauthenticated
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sessions (id,user_id,csrf_token,expires_at) VALUES ($1,$2,$3,$4)`, session.ID, userID, session.CSRFToken, session.ExpiresAt); err != nil {
		return LoginState{}, Session{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LoginState{}, Session{}, err
	}
	return state, session, nil
}

func (r *SQLRepository) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	var session Session
	err := r.store.Pool.QueryRow(ctx, `SELECT s.id,s.user_id,a.steam_id,s.csrf_token,s.expires_at FROM sessions s JOIN steam_accounts a ON a.user_id=s.user_id WHERE s.id=$1`, id).Scan(&session.ID, &session.UserID, &session.SteamID, &session.CSRFToken, &session.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return session, nil
}

func (r *SQLRepository) DeleteSession(ctx context.Context, id uuid.UUID) error {
	_, err := r.store.Pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, id)
	return err
}
