declare module 'steelmatter:demo-data' {
  const data: unknown;
  export default data;
}

// Resolved at bundle time from packages/verifier/node_modules (build.mjs nodePaths).
declare module 'ajv/dist/2020.js' {
  const Ajv2020: unknown;
  export default Ajv2020;
}
declare module 'ajv-formats' {
  const addFormats: unknown;
  export default addFormats;
}
