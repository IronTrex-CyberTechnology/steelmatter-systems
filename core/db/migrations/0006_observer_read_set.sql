-- 0006 observer least privilege (QA-D1-001).
-- The observer's read set is exactly what the readback needs (I02 §8,
-- docs/evidence.md): target.suppliers, target.purchase_orders,
-- target.purchase_order_lines, steelmatter.action_revisions,
-- steelmatter.operations and steelmatter.obligations. Earlier migrations
-- also granted SELECT on schema_info (0001), event_receipts (0003) and
-- observations (0004); the observer never reads them. Applied migrations are
-- immutable (checksummed), so the grants are revoked here.

REVOKE SELECT ON steelmatter.schema_info, steelmatter.event_receipts, steelmatter.observations FROM steelmatter_observer;
