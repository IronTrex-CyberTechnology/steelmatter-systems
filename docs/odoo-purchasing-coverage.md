# Evaluating Odoo 18 purchasing

Start on a customer-approved clone of a self-hosted Odoo 18 Community database. Add the Steelmatter add-ons directory to the Odoo add-ons path, update the Apps list, and install only `steelmatter_coverage` (“Steelmatter purchasing coverage”). This add-on depends on `purchase`. It has no execution endpoint, purchase-model overrides, anchor connection or background job. It collects a configuration snapshot when invoked. It does not observe individual orders in real time.

From an Odoo shell for that database, a Settings administrator can run:

```python
import json
print(json.dumps(env["steelmatter.coverage"].report(), indent=2))
```

Review named UI/RPC buyers, administrators and purchase-order server actions with the customer. Model rights can overstate access to individual records. Unidentified external jobs, server-level changes and direct SQL remain unknown. Keep the output private because it names users. On a database with the R1 executor installed, use its existing `steelmatter.api.sm_coverage` instead.

## Qualifying enforcement on a clone

Install `steelmatter_r1` on a dedicated clone with the supported connector, executor, observer and separate recovery anchor. Installation activates the purchase guard. Grant existing human purchasing users the additional **Steelmatter manual purchasing (unprotected orders)** group only for explicitly agreed human workflows. It grants no Odoo purchase ACLs. Users with purchase rights and without this exception retain the strict guard. Direct purchases by designated buyers are outside engine approval, spend limits and duplicate protection. Never share their accounts or API keys with automation.

Qualify a normal RFQ, edit its lines and confirm it as a designated buyer. Then execute an authorised order through the connector and verify that the buyer cannot change it, reassign its lines or forge a protected reference. Inspect the enforced coverage report for direct buyers, keys, admin access, automations and unknown routes. Administrators, superuser jobs and direct SQL are outside the ordinary-buyer guard. A write-capable identity shared with an agent invalidates a coverage claim for that agent.

Promoting a customer to enforcement requires independent qualification on their configuration, including withdrawal and lost-response recovery. Odoo.sh needs a separate hosted qualification. Confirm engine, anchor and add-on connectivity on that deployment before listing it as supported.
