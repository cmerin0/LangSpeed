package httpserver

import (
	"context"
	"net/http"
)

// leaderboardLimit is how many entries each board returns. The R12 request
// text is not in the repo, so the surface stays deliberately small: a fixed
// top-10 per difficulty rather than a query language nobody asked for - see
// the Step 5 decisions in the README.
const leaderboardLimit = 10

// leaderboardEntryResponse is one ranked row of a board: rank is the 1-based
// position, score the nickname's highest final Score in that difficulty.
type leaderboardEntryResponse struct {
	Rank     int    `json:"rank"`
	Nickname string `json:"nickname"`
	Score    int    `json:"score"`
}

// leaderboardResponse is the full leaderboard payload: one board per
// difficulty, because scores are only comparable within a difficulty (easy
// tops out at 170 points, hard at 390 - decision 16). Every board is always
// present - an empty one as [] rather than null, so the frontend can render
// all three tabs without special cases.
//
// Nicknames appear here on purpose: a leaderboard exists to display them.
// R21.5 keeps them out of log lines, and the Request_Log records only the
// path, so reading the leaderboard leaks nothing into the logs either.
type leaderboardResponse struct {
	Easy   []leaderboardEntryResponse `json:"easy"`
	Medium []leaderboardEntryResponse `json:"medium"`
	Hard   []leaderboardEntryResponse `json:"hard"`
}

// handleLeaderboard implements the Leaderboard read (R12):
//
//	GET /api/leaderboard
//	  -> 200 with the top entries of all three boards, highest score first
//	  -> 503 when the Cache cannot be read
//
// It reads only: no session is touched, so the endpoint needs neither a
// Session_ID nor (yet) authentication - admin auth arrives with Step 6.
func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	resp := leaderboardResponse{
		Easy:   []leaderboardEntryResponse{},
		Medium: []leaderboardEntryResponse{},
		Hard:   []leaderboardEntryResponse{},
	}
	// validDifficulties is the server's own list - the key never carries
	// request input.
	for _, difficulty := range validDifficulties {
		entries, err := s.sessions.TopLeaderboard(ctx, difficulty, leaderboardLimit)
		if err != nil {
			// err is the cache sentinel text only (R21.4).
			s.log.Warn("leaderboard could not be read", "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "the leaderboard could not be retrieved")
			return
		}
		board := make([]leaderboardEntryResponse, 0, len(entries))
		for i, entry := range entries {
			board = append(board, leaderboardEntryResponse{
				Rank:     i + 1,
				Nickname: entry.Nickname,
				Score:    entry.Score,
			})
		}
		switch difficulty {
		case "easy":
			resp.Easy = board
		case "medium":
			resp.Medium = board
		case "hard":
			resp.Hard = board
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
