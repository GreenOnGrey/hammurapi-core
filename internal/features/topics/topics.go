// Package topics is what the topics of the chat have in common
// (FTR.HMR.DEV-0001 arch §4.7): every user has his own topic of the personal
// agent in Nabu per subject (an issue, a feature). Hammurapi keeps the link
// "user + subject → topic"; the topic is created by the first message only.
package topics

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/nabuconn"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// MaxTitle bounds the title of a topic in Nabu.
const MaxTitle = 120

// Title is the title of a topic: "<key> — <title>", at most MaxTitle
// characters with an ellipsis.
func Title(key, title string) string {
	t := key + " — " + strings.TrimSpace(title)
	if utf8.RuneCountInString(t) <= MaxTitle {
		return t
	}
	r := []rune(t)
	return strings.TrimRight(string(r[:MaxTitle-1]), " ") + "…"
}

// Links is a table of links "user + subject → topic".
type Links struct {
	// Table and Column are constants of the code: the table and its subject column.
	Table, Column string
	// Lock is the prefix of the advisory lock of a pair.
	Lock string
}

// Issue and Feature are the links of the two kinds of subject.
var (
	Issue   = Links{Table: "issue_topics", Column: "issue_id", Lock: "issue-topic:"}
	Feature = Links{Table: "feature_topics", Column: "feature_id", Lock: "feature-topic:"}
)

// ConversationOf returns the topic of the user for the subject ("" — none).
func (l Links) ConversationOf(ctx context.Context, q postgres.Querier, userID, subject uuid.UUID) (string, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT conversation_id FROM `+l.Table+` WHERE user_id = $1 AND `+l.Column+` = $2`, userID, subject).Scan(&id)
	if postgres.IsNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Forget drops the link to a topic that is gone in Nabu.
func (l Links) Forget(ctx context.Context, q postgres.Querier, conversationID string) {
	if _, err := q.Exec(ctx, `DELETE FROM `+l.Table+` WHERE conversation_id = $1`, conversationID); err != nil {
		slog.WarnContext(ctx, "forget a topic", "table", l.Table, "conversation_id", conversationID, "err", err)
	}
}

// Ensure returns the topic of the pair and creates it when there is none. The
// pair is locked while the topic is created, so two tabs never make two
// topics; created runs for a topic made by this call.
func (l Links) Ensure(ctx context.Context, pool *pgxpool.Pool, client *nabu.Client, userID uuid.UUID, email string, subject uuid.UUID, title string, created func()) (string, error) {
	var conv string
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || $2::text || ':' || $3::text, 0))`, l.Lock, userID, subject); err != nil {
			return err
		}
		var err error
		if conv, err = l.ConversationOf(ctx, tx, userID, subject); err != nil || conv != "" {
			return err
		}
		c, err := client.CreateTopic(ctx, email, title)
		if err != nil {
			return nabuconn.MapErr(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+l.Table+` (user_id, `+l.Column+`, conversation_id) VALUES ($1,$2,$3)`, userID, subject, c.ID); err != nil {
			return err
		}
		conv = c.ID
		if created != nil {
			created()
		}
		return nil
	})
	return conv, err
}

// Link is a topic of a subject.
type Link struct {
	User uuid.UUID
	Conv string
}

// All lists the topics of a subject.
func (l Links) All(ctx context.Context, q postgres.Querier, subject uuid.UUID) ([]Link, error) {
	rows, err := q.Query(ctx, `SELECT user_id, conversation_id FROM `+l.Table+` WHERE `+l.Column+` = $1`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var x Link
		var conv uuid.UUID
		if err := rows.Scan(&x.User, &conv); err != nil {
			return nil, err
		}
		x.Conv = conv.String()
		out = append(out, x)
	}
	return out, rows.Err()
}

// ErrNoEmail: the owner of a topic lost the email, Nabu is reached by it.
var ErrNoEmail = errors.New("the user has no email")
