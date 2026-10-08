-- 0004: per-tongue-twister breakdown of a finished game (Requirement 2.6).
-- Columns: id, game_record_id, tongue_twister_id, attempt_count,
--          time_taken_ms, success.
CREATE TABLE attempt_details (
    id                bigserial PRIMARY KEY,
    game_record_id    bigint  NOT NULL REFERENCES game_records (id) ON DELETE CASCADE,
    tongue_twister_id bigint  NOT NULL REFERENCES tongue_twisters (id) ON DELETE CASCADE,
    attempt_count     integer NOT NULL CHECK (attempt_count >= 0),
    time_taken_ms     integer NOT NULL CHECK (time_taken_ms >= 0),
    success           boolean NOT NULL
);

-- One Attempt_Detail row per tongue twister inside a game record (R10.2, R11.2).
CREATE INDEX attempt_details_game_record_idx ON attempt_details (game_record_id);
