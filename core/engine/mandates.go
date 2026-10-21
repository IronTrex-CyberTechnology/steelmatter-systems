package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/auth"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/failpoint"
)

// MandateRequest is the owner's request (server-owned fields excluded).
type MandateRequest struct {
	ActorIDs    []string
	Audience    string
	SupplierIDs []string
	DeliveryIDs []string
	LimitMinor  int64
	ExpiresAt   string
}

// Buckets is the allowance ledger for a mandate.
type Buckets struct {
	LimitMinor      int64 `json:"limit_minor"`
	AvailableMinor  int64 `json:"available_minor"`
	ReservedMinor   int64 `json:"reserved_minor"`
	CommittedMinor  int64 `json:"committed_minor"`
	UnresolvedMinor int64 `json:"unresolved_minor"`
}

// MandateView is returned by create.
type MandateView struct {
	Mandate       json.RawMessage `json:"mandate"`
	MandateDigest string          `json:"mandate_digest"`
	MandateJWS    string          `json:"mandate_jws"`
	Status        string          `json:"status"`
	Buckets       Buckets         `json:"buckets"`
}

// RevokeView is returned by revoke.
type RevokeView struct {
	MandateID    string  `json:"mandate_id"`
	Status       string  `json:"status"`
	ControlEpoch int64   `json:"control_epoch"`
	RevokedAt    string  `json:"revoked_at"`
	Buckets      Buckets `json:"buckets"`
}

// CreateMandate creates a bounded mandate with revision 1 and control epoch 1,
// signed with the mandate key (201).
func (e *Engine) CreateMandate(ctx context.Context, p auth.Principal, r MandateRequest) (*MandateView, error) {
	if p.Kind != auth.Owner {
		return nil, fault("FORBIDDEN", "only an owner can create a mandate")
	}
	expires, err := contract.ParseTimestamp(r.ExpiresAt)
	if err != nil {
		return nil, fault("INVALID_TIMESTAMP", "%v", err)
	}
	var now time.Time
	if err := e.Pool.QueryRow(ctx, `SELECT date_trunc('second', clock_timestamp())`).Scan(&now); err != nil {
		return nil, unavailable(err)
	}
	if !expires.After(now) {
		return nil, fault("INVALID_TIME_RANGE", "expires_at must be in the future")
	}
	m := contract.Mandate{
		SchemaVersion: contract.SchemaVersion, CryptoProfile: contract.CryptoProfile,
		MandateID: newID("mdt"), TenantID: p.Tenant, IssuerID: p.ID, Revision: 1, ControlEpoch: 1,
		ActorIDs: r.ActorIDs, Audience: r.Audience, Operation: "purchase_order.issue",
		SupplierIDs: r.SupplierIDs, DeliveryIDs: r.DeliveryIDs, Currency: contract.Currency,
		LimitMinor: r.LimitMinor, IssuedAt: ts(now), ExpiresAt: ts(expires),
	}
	raw, _ := json.Marshal(m)
	canonical, digest, err := contract.CanonicalDigest(contract.KindMandate, raw)
	if err != nil {
		if ce, ok := err.(*contract.Error); ok {
			return nil, fault(string(ce.Code), "%s", ce.Detail)
		}
		return nil, err
	}
	signer, err := e.signer("SM-MANDATE+JWS")
	if err != nil {
		return nil, err
	}
	jws, err := contract.Sign(contract.KindMandate, canonical, signer)
	if err != nil {
		return nil, err
	}
	err = e.inTx(ctx, "", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO steelmatter.mandates (tenant_id, mandate_id, revision, control_epoch, status, owner_principal,
			audience, actor_ids, supplier_ids, delivery_ids, currency, limit_minor, available_minor, reserved_minor, committed_minor,
			payload_canonical, mandate_digest, mandate_jws, issued_at, expires_at)
			VALUES ($1, $2, 1, 1, 'ACTIVE', $3, $4, $5, $6, $7, 'INR', $8, $8, 0, 0, $9, $10, $11, $12, $13)`,
			m.TenantID, m.MandateID, p.ID, m.Audience, m.ActorIDs, m.SupplierIDs, m.DeliveryIDs, m.LimitMinor,
			canonical, digest, jws, now, expires)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &MandateView{Mandate: canonical, MandateDigest: digest, MandateJWS: jws, Status: "ACTIVE",
		Buckets: Buckets{LimitMinor: m.LimitMinor, AvailableMinor: m.LimitMinor}}, nil
}

// RevokeMandate revokes under the mandate lock, increments the control epoch
// and releases unused reservations. Repeating it returns the same state (200).
// Revocation after commit cannot undo a committed order (I6).
func (e *Engine) RevokeMandate(ctx context.Context, p auth.Principal, mandateID string) (*RevokeView, error) {
	if p.Kind != auth.Owner {
		return nil, fault("FORBIDDEN", "only an owner can revoke a mandate")
	}
	var v RevokeView
	err := e.inTx(ctx, "", func(tx pgx.Tx) error {
		var owner, status string
		var epoch int64
		err := tx.QueryRow(ctx, `SELECT owner_principal, status, control_epoch FROM steelmatter.mandates
			WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`, p.Tenant, mandateID).Scan(&owner, &status, &epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault("NOT_FOUND", "mandate not found")
		}
		if err != nil {
			return err
		}
		if owner != p.ID {
			return fault("NOT_FOUND", "mandate not found") // no disclosure to other owners
		}
		if err := failpoint.Inject(ctx, failpoint.RevokeAfterLock); err != nil {
			return err
		}
		if status == "ACTIVE" {
			// Issuance rows after the mandate row, in primary-key order.
			var released int64
			rows, err := tx.Query(ctx, `SELECT permit_id, amount_minor FROM steelmatter.issuances
				WHERE tenant_id = $1 AND mandate_id = $2 AND status IN ('UNSIGNED', 'ACTIVE') ORDER BY permit_id FOR UPDATE`, p.Tenant, mandateID)
			if err != nil {
				return err
			}
			var ids []string
			for rows.Next() {
				var id string
				var amt int64
				if err := rows.Scan(&id, &amt); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
				released += amt
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(ids) > 0 {
				if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'REVOKED', released_at = clock_timestamp()
					WHERE tenant_id = $1 AND permit_id = ANY($2)`, p.Tenant, ids); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET status = 'REVOKED', control_epoch = control_epoch + 1,
				revoked_at = date_trunc('second', clock_timestamp()), reserved_minor = reserved_minor - $3, available_minor = available_minor + $3
				WHERE tenant_id = $1 AND mandate_id = $2`, p.Tenant, mandateID, released); err != nil {
				return err
			}
			// Record the revocation event in the same transaction (document 03; F-15).
			if ids == nil {
				ids = []string{}
			}
			payload, _ := json.Marshal(map[string]any{"mandate_id": mandateID, "control_epoch": epoch + 1, "owner": p.ID,
				"released_minor": released, "released_permit_ids": ids})
			if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at)
				VALUES ($1, $2, 'mandate.revoked', $3, $4, clock_timestamp())`, newID("evt"), p.Tenant, mandateID, payload); err != nil {
				return err
			}
		}
		var revokedAt time.Time
		v.MandateID = mandateID
		err = tx.QueryRow(ctx, `SELECT status, control_epoch, revoked_at, limit_minor, available_minor, reserved_minor, committed_minor, unresolved_minor
			FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2`, p.Tenant, mandateID).
			Scan(&v.Status, &v.ControlEpoch, &revokedAt, &v.Buckets.LimitMinor, &v.Buckets.AvailableMinor,
				&v.Buckets.ReservedMinor, &v.Buckets.CommittedMinor, &v.Buckets.UnresolvedMinor)
		if err == nil {
			v.RevokedAt = ts(revokedAt)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}
