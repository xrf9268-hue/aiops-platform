package runner

// CodexProtocolVersion is the single source of truth for the Codex CLI version
// this build targets. The vendored app-server protocol schema
// (codexProtocolSchemaFile), the Dockerfile CODEX_CLI_VERSION arg, and the
// doctor version check are all pinned to it; cmd/worker's
// TestCodexVersionPinParity fails the build if any of them drift. Bump it only
// via scripts/refresh-codex-schema.sh, which regenerates the schema from the
// matching codex binary.
const CodexProtocolVersion = "0.144.4"

// codexProtocolSchemaFile is the vendored JSON Schema bundle produced by
// `codex app-server generate-json-schema --out <dir> --experimental` for
// CodexProtocolVersion, used by the schema contract test. It MUST be generated
// with --experimental: we enable the experimental API (initialize
// capabilities.experimentalApi) and send experimental request fields such as
// thread/start dynamicTools, which the default (non-experimental) schema export
// strips — validating against a non-experimental bundle would falsely reject
// dynamicTools.
const codexProtocolSchemaFile = "codex_app_server_protocol_v0_144_4.v2.schemas.json"

//go:generate sh -c "cd ../.. && scripts/refresh-codex-schema.sh"
