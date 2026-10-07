-- Records that a run's plan was the deterministic fallback rather than something
-- the model produced.
--
-- A fallback plan is built from the user's own goal, so without this flag it is
-- indistinguishable from a real plan at a glance and a user could approve it
-- believing the model had reasoned about the request.
ALTER TABLE agent_runs
    ADD COLUMN plan_fallback boolean NOT NULL DEFAULT false;