import { bindAction, compareObservation } from '../src/index.mjs';
import { createFixture } from './memory-adapter.mjs';

const fixture = createFixture();
const action = bindAction(fixture.manifest, fixture.request);
fixture.loseNextResponse();
let execution;
try {
  execution = await fixture.adapter.execute(action);
} catch {
  // The outcome is unknown, not failed. Do not execute again or release
  // authority capacity. This fixture demonstrates only destination recovery;
  // a real authority service must own the durable reservation state.
  console.log('Execution response lost: UNKNOWN. No retry.');
  execution = await fixture.adapter.reconcile(action);
}
const observation = await fixture.adapter.observe(action);
console.log(JSON.stringify({
  synthetic: true, authorization_performed: false, signed_evidence: false,
  execution: execution.status,
  observation: compareObservation(fixture.manifest, action, execution, observation),
  destination_writes: fixture.writeCount(),
}, null, 2));
