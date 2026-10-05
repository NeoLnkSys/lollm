-- Per-combo compression toggle (spec section 3.5): '' = inherit the global
-- compression.default setting. Values: off | partial | full (validated in code).
ALTER TABLE combos ADD COLUMN compression TEXT NOT NULL DEFAULT '';
