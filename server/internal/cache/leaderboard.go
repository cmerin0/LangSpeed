package cache

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// The Leaderboard (Requirement 12) lives in Redis as one sorted set per
// difficulty under leaderboard:{difficulty} (decision 16: scores are only
// comparable within a difficulty - easy tops out at 170 points, hard at 390).
//
// Members are nicknames, scores the final Score of a finished game (R10).
// ZADD's GT flag keeps only a nickname's highest score (R12.6), and equal
// scores of different nicknames simply sit next to each other: ordering is
// by score alone and no tie-break is ever computed (decision 5, resolved by
// the owner). The key is built from the validated session difficulty or the
// server's own difficulty list - never from raw request input. Entries never
// expire: a leaderboard is history, not live game state.

// leaderboardKey returns the sorted-set key for one difficulty.
func leaderboardKey(difficulty string) string { return "leaderboard:" + difficulty }

// LeaderboardEntry is one row of a difficulty's board: the nickname and the
// highest final Score it achieved in that difficulty.
type LeaderboardEntry struct {
	Nickname string
	Score    int
}

// SaveLeaderboardScore folds a finished game's result (R10) into the
// leaderboard (R12): score is the final Score, kept only if it is higher than
// the nickname's previous best (GT), so a later, worse run can never demote a
// player. An unreachable Cache surfaces as ErrUnavailable for the HTTP layer
// to answer 503.
func (s *Store) SaveLeaderboardScore(ctx context.Context, difficulty, nickname string, score int) error {
	err := s.client.ZAddArgs(ctx, leaderboardKey(difficulty), redis.ZAddArgs{
		Members: []redis.Z{{Score: float64(score), Member: nickname}},
		GT:      true,
	}).Err()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// TopLeaderboard returns up to limit entries of one difficulty's board,
// highest score first. The result is always non-nil - an empty board yields
// an empty slice, so HTTP responses render [] rather than null. An
// unreachable Cache surfaces as ErrUnavailable (R3.6 style).
func (s *Store) TopLeaderboard(ctx context.Context, difficulty string, limit int) ([]LeaderboardEntry, error) {
	if limit < 1 {
		limit = 1
	}
	members, err := s.client.ZRevRangeWithScores(ctx, leaderboardKey(difficulty), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	entries := make([]LeaderboardEntry, 0, len(members))
	for _, member := range members {
		nickname, ok := member.Member.(string)
		if !ok {
			// Members are the nicknames this package writes; anything else
			// would be foreign data under our key, so skip it rather than
			// invent a display name.
			continue
		}
		entries = append(entries, LeaderboardEntry{Nickname: nickname, Score: int(member.Score)})
	}
	return entries, nil
}
