-- 0001: tongue_twisters catalogue (Requirement 2.3).
-- Columns: id, text, difficulty, active, created_at, updated_at.
CREATE TABLE tongue_twisters (
    id         bigserial PRIMARY KEY,
    text       varchar(500) NOT NULL,
    difficulty varchar(8)   NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    active     boolean      NOT NULL DEFAULT true,
    created_at timestamptz  NOT NULL DEFAULT now(),
    updated_at timestamptz  NOT NULL DEFAULT now()
);

-- Speeds up "next tongue-twister" lookups filtered by difficulty + active (R4, R6).
CREATE INDEX tongue_twisters_difficulty_active_idx ON tongue_twisters (difficulty, active);
