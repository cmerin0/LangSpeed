-- 0003: permanent results of finished games (Requirement 2.5).
-- Columns: id, nickname, final_score, difficulty, started_at, ended_at.
CREATE TABLE game_records (
    id          bigserial PRIMARY KEY,
    nickname    varchar(32) NOT NULL,
    final_score integer     NOT NULL,
    difficulty  varchar(8)  NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    started_at  timestamptz NOT NULL,
    ended_at    timestamptz NOT NULL
);

-- Supports score-ordered queries over historical results.
CREATE INDEX game_records_final_score_idx ON game_records (final_score DESC);
CREATE INDEX game_records_nickname_idx ON game_records (nickname);
