-- 0015 R1: a committed order deleted at the destination (R1-DEF-20).
--
-- ORDER_DELETED_AT_DESTINATION marks a committed operation whose Odoo order
-- was deleted by someone with rights to do so; the destination journal
-- records the deletion, so this is not a restore. It stays committed (no
-- refund) and the flag is set once and never cleared, like
-- ORDER_MISSING_AFTER_RESTORE. Only this operation is affected: writes stay
-- on for every other operation.

ALTER TABLE steelmatter.dispatches DROP CONSTRAINT IF EXISTS dispatches_flag_check;
ALTER TABLE steelmatter.dispatches ADD CONSTRAINT dispatches_flag_check
    CHECK (flag IN ('ORDER_MISSING_AFTER_RESTORE', 'ORDER_DELETED_AT_DESTINATION'));
