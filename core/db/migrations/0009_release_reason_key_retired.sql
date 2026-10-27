-- 0009 KEY_RETIRED release reason (SEC-DEF-001, ADR-004). An identical
-- evaluation retry whose live ACTIVE permit was signed by a key that is now
-- retired releases that issuance with this reason and issues a new permit
-- signed by the current key, in the same transaction. Admission refuses
-- permits from retired keys, so the released permit was unusable; its
-- capacity is released and re-reserved, never duplicated.

ALTER TABLE steelmatter.issuances DROP CONSTRAINT issuances_release_reason_check;
ALTER TABLE steelmatter.issuances ADD CONSTRAINT issuances_release_reason_check
    CHECK (release_reason IN ('EXPIRED', 'REVOKED', 'AMENDED', 'RECOVERED_UNUSED', 'KEY_RETIRED'));
