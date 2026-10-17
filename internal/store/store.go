package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	pgvector "github.com/pgvector/pgvector-go"
	dbgen "next-game-crush/internal/store/sqlc"
)

const EmbeddingDimensions = 1536

type Store struct {
	Pool    *pgxpool.Pool
	Queries *dbgen.Queries
}

type Game struct {
	ID         uuid.UUID
	SteamAppID int64
	Name       string
	Source     string
	FetchedAt  time.Time
}

type GameEmbedding struct {
	GameID      uuid.UUID
	Model       string
	ContentHash string
	Vector      []float32
}

type EmbeddingMatch struct {
	GameID         uuid.UUID
	Model          string
	CosineDistance float64
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if databaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	migrationDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open migration connection: %w", err)
	}
	if err := migrate(ctx, migrationDB); err != nil {
		migrationDB.Close()
		return nil, err
	}
	if err := migrationDB.Close(); err != nil {
		return nil, fmt.Errorf("close migration connection: %w", err)
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{Pool: pool, Queries: dbgen.New(pool)}, nil
}

func (s *Store) Close() {
	if s != nil && s.Pool != nil {
		s.Pool.Close()
	}
}

func (s *Store) UpsertGame(ctx context.Context, game Game) error {
	_, err := s.Queries.UpsertGame(ctx, dbgen.UpsertGameParams{
		ID: uuidToPG(game.ID), SteamAppID: game.SteamAppID, Name: game.Name,
		Source: game.Source, LastSuccessfulFetchAt: timestamptz(game.FetchedAt),
	})
	return err
}

func (s *Store) PutGameEmbedding(ctx context.Context, embedding GameEmbedding) error {
	if len(embedding.Vector) != EmbeddingDimensions {
		return fmt.Errorf("embedding must contain exactly %d dimensions, got %d", EmbeddingDimensions, len(embedding.Vector))
	}
	return s.Queries.PutGameEmbedding(ctx, dbgen.PutGameEmbeddingParams{
		GameID: uuidToPG(embedding.GameID), Model: embedding.Model,
		InputContentHash: embedding.ContentHash, Embedding: pgvector.NewVector(embedding.Vector),
	})
}

func (s *Store) GetGameEmbedding(ctx context.Context, gameID uuid.UUID, model string) (GameEmbedding, error) {
	row, err := s.Queries.GetGameEmbedding(ctx, dbgen.GetGameEmbeddingParams{GameID: uuidToPG(gameID), Model: model})
	if err != nil {
		return GameEmbedding{}, err
	}
	id, err := uuidFromPG(row.GameID)
	if err != nil {
		return GameEmbedding{}, err
	}
	return GameEmbedding{GameID: id, Model: row.Model, ContentHash: row.InputContentHash, Vector: row.Embedding.Slice()}, nil
}

func (s *Store) NearestGameEmbeddings(ctx context.Context, model string, vector []float32, limit int32) ([]EmbeddingMatch, error) {
	if model == "" {
		return nil, errors.New("embedding model is required")
	}
	if len(vector) != EmbeddingDimensions {
		return nil, fmt.Errorf("query embedding must contain exactly %d dimensions, got %d", EmbeddingDimensions, len(vector))
	}
	if limit <= 0 {
		return nil, errors.New("result limit must be positive")
	}
	rows, err := s.Queries.NearestGameEmbeddings(ctx, dbgen.NearestGameEmbeddingsParams{
		QueryEmbedding: pgvector.NewVector(vector), Model: model, ResultLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	result := make([]EmbeddingMatch, 0, len(rows))
	for _, row := range rows {
		id, err := uuidFromPG(row.GameID)
		if err != nil {
			return nil, err
		}
		result = append(result, EmbeddingMatch{GameID: id, Model: row.Model, CosineDistance: row.CosineDistance})
	}
	return result, nil
}

func uuidToPG(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: value, Valid: true}
}

func uuidFromPG(value pgtype.UUID) (uuid.UUID, error) {
	if !value.Valid {
		return uuid.Nil, errors.New("unexpected null UUID")
	}
	return uuid.UUID(value.Bytes), nil
}

func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
