// node:test reporter emitting `go test -json` events so the steelmatter-ci
// aggregate gate (tools/ciagg) evaluates browser demo results like Go results.
import path from 'node:path';

export default async function* gojson(source) {
  const pkg = 'demo';
  for await (const { type, data } of source) {
    if (type !== 'test:pass' && type !== 'test:fail') continue;
    if (data.details?.type === 'suite') continue;
    const file = data.file ? path.basename(data.file) : 'unknown';
    const name = `${file}/${data.name}`;
    let action = type === 'test:pass' ? 'pass' : 'fail';
    if (data.skip || data.todo) action = 'skip';
    if (action === 'fail') {
      const err = data.details?.error;
      const msg = err ? String(err.cause?.message ?? err.message ?? err) : 'failed';
      yield JSON.stringify({ Action: 'output', Package: pkg, Test: name, Output: msg + '\n' }) + '\n';
    }
    yield JSON.stringify({ Action: action, Package: pkg, Test: name }) + '\n';
  }
}
