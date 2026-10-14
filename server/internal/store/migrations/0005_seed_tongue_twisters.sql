-- 0005: seed a small playable catalogue of tongue-twisters (Requirement 4, 6).
-- Without rows, the next-twister endpoint could only ever answer "no content
-- is available for the selected difficulty" (R4.6), so a fresh installation
-- must ship with content to stay playable after a single `docker compose up`.
--
-- The whole batch is skipped when the catalogue already contains a row, so a
-- database an admin has populated later (R14, Step 6) is never touched. Every
-- seeded row is active - selection only ever picks active rows (R4.5) - and
-- each text stays within the 500-character column limit (R14.3).
INSERT INTO tongue_twisters (text, difficulty, active)
SELECT v.text, v.difficulty, true
FROM (VALUES
    ('She sells seashells by the seashore.', 'easy'),
    ('Peter Piper picked a peck of pickled peppers.', 'easy'),
    ('How can a clam cram in a clean cream can?', 'easy'),
    ('Toy boat, toy boat, toy boat.', 'easy'),
    ('Unique New York, unique New York, unique New York.', 'easy'),
    ('Red lorry, yellow lorry, red lorry, yellow lorry.', 'easy'),
    ('Fuzzy Wuzzy was a bear. Fuzzy Wuzzy had no hair.', 'easy'),
    ('A proper copper coffee pot.', 'easy'),
    ('Big black bugs bled blue black blood.', 'easy'),
    ('I saw a saw that saw all I ever saw.', 'easy'),
    ('A box of biscuits, a box of mixed biscuits.', 'easy'),
    ('Betty Botter bought some butter, but she said the butter was bitter.', 'easy'),
    ('I scream, you scream, we all scream for ice cream.', 'easy'),
    ('Four fine fresh fish for you.', 'easy'),
    ('A happy hippo hopped hopefully home.', 'easy'),
    ('Which witch wished which wicked wish?', 'easy'),
    ('Six slimy snails slid slowly seaward.', 'easy'),
    ('Irish wristwatch, Swiss wristwatch.', 'medium'),
    ('Which wristwatches are Swiss wristwatches?', 'medium'),
    ('The sixth sick sheik''s sixth sheep is sick.', 'medium'),
    ('If two witches were watching two watches, which witch would watch which watch?', 'medium'),
    ('I wish to wash my Irish wristwatch.', 'medium'),
    ('Seven silver swans swam swiftly seawards.', 'medium'),
    ('No nose knows like a gnome knows his nose.', 'medium'),
    ('Flash message, fresh message, flash message, fresh message.', 'medium'),
    ('Brisk brave brigadiers brandished bright broad blades.', 'medium'),
    ('Cheswick''s church chest chokes crisply.', 'medium'),
    ('A skunk sat on a stump and thunk the stump stunk, but the stump thunk the skunk stunk.', 'medium'),
    ('Lesser leather never weathered wetter weather better.', 'medium'),
    ('The great Greek grape growers grow great Greek grapes.', 'medium'),
    ('Six sleek swans swam swiftly southwards.', 'medium'),
    ('The queen in green screamed as the clean screen gleamed.', 'medium'),
    ('The thirty-three thieves thought that they thrilled the throne throughout Thursday.', 'hard'),
    ('Buffalo buffalo Buffalo buffalo buffalo buffalo Buffalo buffalo.', 'hard'),
    ('If a blacksmith fixes a frosty forged horseshoe, is it a frosty forged horseshoe that the blacksmith fixes?', 'hard'),
    ('Which witch switched the Swiss wristwatch straps?', 'hard'),
    ('Can you can a can as a canner can can a can?', 'hard'),
    ('A big black bug bit a big black bear, but did the big black bug bite the big black bear?', 'hard'),
    ('How many sheets could a sheet slitter slit if sheet slitters could slit sheets?', 'hard'),
    ('Real rugged rear wheels on the wrecked railcar rattle relentlessly.', 'hard'),
    ('The bleak blade blunted the blacksmith''s bright blue blade before the blacksmith could blink.', 'hard'),
    ('A really leery Larry rarely chews raw rural rarely ripe red rhubarb.', 'hard'),
    ('Three free throws thrilled the thrifty thrill-seeker through Thursday''s thundering thunderstorm.', 'hard'),
    ('Crisp crusts crackle as clever cooks quickly crush crunchy crackers.', 'hard'),
    ('Which wristwatch would a Swiss witch switch if a Swiss witch could switch wristwatches?', 'hard')
) AS v(text, difficulty)
WHERE NOT EXISTS (SELECT 1 FROM tongue_twisters);
