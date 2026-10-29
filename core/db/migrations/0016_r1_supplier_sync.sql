-- 0016 R1: the destination is the supplier revision source (R1-DEF-03).
--
-- With a real destination (Odoo), the protected supplier revision lives in
-- Odoo (res.partner.sm_revision). The engine copies it into target.suppliers
-- before creating an instruction and before evaluation, through this one
-- controlled function; the executor still has no INSERT or UPDATE on the
-- table itself.
--
-- The copy only moves forward: a missing supplier is added as ACTIVE at the
-- destination's revision, and an existing one takes the higher of the two
-- revisions. A revision written backwards at the destination never makes an
-- old authorisation look current again (the destination's own stale check
-- then refuses the order). A SUSPENDED supplier stays suspended.

CREATE FUNCTION target.sync_supplier_revision(p_tenant text, p_supplier text, p_revision bigint)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, target AS $$
DECLARE
    r bigint;
BEGIN
    IF p_revision IS NULL OR p_revision < 1 THEN
        RAISE EXCEPTION 'supplier %: revision % is not valid', p_supplier, p_revision USING ERRCODE = 'SM001';
    END IF;
    INSERT INTO target.suppliers (tenant_id, supplier_id, revision, status)
    VALUES (p_tenant, p_supplier, p_revision, 'ACTIVE')
    ON CONFLICT (tenant_id, supplier_id) DO UPDATE SET revision = GREATEST(target.suppliers.revision, EXCLUDED.revision)
    RETURNING revision INTO r;
    RETURN r;
END $$;

REVOKE ALL ON FUNCTION target.sync_supplier_revision(text, text, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION target.sync_supplier_revision(text, text, bigint) TO steelmatter_executor;
