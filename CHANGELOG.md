# Changelog

## [0.1.17](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.16...v0.1.17) (2026-07-18)


### Bug Fixes

* **observability:** scope claim token budget ([#1124](https://github.com/xrf9268-hue/aiops-platform/issues/1124)) ([b1180b9](https://github.com/xrf9268-hue/aiops-platform/commit/b1180b93b8b36d45a8f4a419cbdf0243d7e44f11))
* **orchestrator:** classify issue refresh outcomes ([#1121](https://github.com/xrf9268-hue/aiops-platform/issues/1121)) ([16d9105](https://github.com/xrf9268-hue/aiops-platform/commit/16d91050d650ab5e6c846340ff421674a5862ac3))
* **orchestrator:** reconcile before dispatch gates ([#1119](https://github.com/xrf9268-hue/aiops-platform/issues/1119)) ([054bab4](https://github.com/xrf9268-hue/aiops-platform/commit/054bab42ca9b477e6b93d6c0b94aa7fd2d2cf34d))
* **runner:** align Codex temp root and sandbox guidance ([#1123](https://github.com/xrf9268-hue/aiops-platform/issues/1123)) ([5f813c8](https://github.com/xrf9268-hue/aiops-platform/commit/5f813c806b5128fe8444ec96a8e6ab353afbfbc2))
* **runner:** restore schema-aligned continuation ([#1132](https://github.com/xrf9268-hue/aiops-platform/issues/1132)) ([9600298](https://github.com/xrf9268-hue/aiops-platform/commit/9600298626642c4cc78e254d762a1e0557311177)), closes [#1129](https://github.com/xrf9268-hue/aiops-platform/issues/1129)
* **worker:** remove only confirmed terminal workspaces ([#1122](https://github.com/xrf9268-hue/aiops-platform/issues/1122)) ([6ccf080](https://github.com/xrf9268-hue/aiops-platform/commit/6ccf0800fc6e68f30a63ebcf78788672e3f28a62))

## [0.1.16](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.15...v0.1.16) (2026-07-15)


### Bug Fixes

* **doctor:** fail on Codex protocol drift ([#1106](https://github.com/xrf9268-hue/aiops-platform/issues/1106)) ([4b9f053](https://github.com/xrf9268-hue/aiops-platform/commit/4b9f0535fe5d396a6c3b38178023001b5d913f62))
* **observability:** retain ended session usage ([#1104](https://github.com/xrf9268-hue/aiops-platform/issues/1104)) ([33bb17e](https://github.com/xrf9268-hue/aiops-platform/commit/33bb17e58fe8cd4d2ff8450757ff78e984409f1f))
* **orchestrator:** make token usage folding exact ([#1103](https://github.com/xrf9268-hue/aiops-platform/issues/1103)) ([dcf47cd](https://github.com/xrf9268-hue/aiops-platform/commit/dcf47cd936c4bc92f33a1ab572e619c1dd046197))
* **workflow:** close reviewed issues from squash commits ([#1109](https://github.com/xrf9268-hue/aiops-platform/issues/1109)) ([9702dc0](https://github.com/xrf9268-hue/aiops-platform/commit/9702dc07732a88f7547317678baec4e9db178435))
* **workflow:** make stable-head reviewer retries idempotent ([#1094](https://github.com/xrf9268-hue/aiops-platform/issues/1094)) ([480844b](https://github.com/xrf9268-hue/aiops-platform/commit/480844b2f60baec5ebe44e82ee5710343d2c044a))

## [0.1.15](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.14...v0.1.15) (2026-07-03)


### Bug Fixes

* **dashboard:** label codex window durations ([3f37806](https://github.com/xrf9268-hue/aiops-platform/commit/3f37806f7bcd2a6cad9661753aaefb9d5c8a7c69))
* **dashboard:** show days for long resets ([b3322c0](https://github.com/xrf9268-hue/aiops-platform/commit/b3322c0584205d3ce2e8275e247a79d5bb8a8551))
* **gitea:** reuse default tracker HTTP client ([#1082](https://github.com/xrf9268-hue/aiops-platform/issues/1082)) ([feb0d56](https://github.com/xrf9268-hue/aiops-platform/commit/feb0d560d9bf2e6fd7e142a8b2e327651f8811fb))
* **orchestrator:** avoid dispatch panic hangs ([8edca8c](https://github.com/xrf9268-hue/aiops-platform/commit/8edca8cf35745ee607e7abd6bdb7ad9c2f628460))
* **orchestrator:** avoid duplicate stall cancels ([16de87b](https://github.com/xrf9268-hue/aiops-platform/commit/16de87b0ab686707273ff07112923f6a2b7ac5ea))
* **orchestrator:** recover dispatch spawn panics ([#1081](https://github.com/xrf9268-hue/aiops-platform/issues/1081)) ([8edca8c](https://github.com/xrf9268-hue/aiops-platform/commit/8edca8cf35745ee607e7abd6bdb7ad9c2f628460))
* **tracker:** cap success JSON decoding ([f1103f5](https://github.com/xrf9268-hue/aiops-platform/commit/f1103f50c8a7293fab8124fb3d7d5177eb0bd0b3))
* **tui:** render codex percent rate windows ([b14dff2](https://github.com/xrf9268-hue/aiops-platform/commit/b14dff255bb3fc1727f2e807f9b31f2537ef0dc1))
* **tui:** show human-scale durations ([ede11f3](https://github.com/xrf9268-hue/aiops-platform/commit/ede11f3f7e44da78b4fd0e7babae2ae0e9ec4224))

## [0.1.14](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.13...v0.1.14) (2026-07-03)


### Bug Fixes

* **github:** do not synthesize blocker lookup failures ([#1060](https://github.com/xrf9268-hue/aiops-platform/issues/1060)) ([e67858a](https://github.com/xrf9268-hue/aiops-platform/commit/e67858a80aa3849b63da31f9760deaed88488d35))
* **linear:** fail closed when listing blocker lookup fails ([#1065](https://github.com/xrf9268-hue/aiops-platform/issues/1065)) ([5f7590e](https://github.com/xrf9268-hue/aiops-platform/commit/5f7590ed2fd9e81ad81c3bfef3c95f28445c5c26))
* **orchestrator:** preserve quota retry-after on reschedule ([#1066](https://github.com/xrf9268-hue/aiops-platform/issues/1066)) ([92fc8a2](https://github.com/xrf9268-hue/aiops-platform/commit/92fc8a2f8ff6ce82457bb03f270aebf19fabb9b6))
* **runner:** decouple codex stdout draining from the busy consumer and bound stdin writes ([da44b9f](https://github.com/xrf9268-hue/aiops-platform/commit/da44b9f6cd1ea5c85f3d8798686a5a43499c2f91)), closes [#1033](https://github.com/xrf9268-hue/aiops-platform/issues/1033)
* **runner:** preserve inherited codex environment ([#1058](https://github.com/xrf9268-hue/aiops-platform/issues/1058)) ([39b63e8](https://github.com/xrf9268-hue/aiops-platform/commit/39b63e858c6cacef07e3b1cf1ca7a0eab6c2b886))
* **runner:** scope CODEX_HOME to codex app-server ([03012ed](https://github.com/xrf9268-hue/aiops-platform/commit/03012ed36f12ee6f96a65ada6682df5de5db714f)), closes [#1062](https://github.com/xrf9268-hue/aiops-platform/issues/1062)
* **security:** redact state error credentials ([5a4003b](https://github.com/xrf9268-hue/aiops-platform/commit/5a4003b9fa91d8758dc2b90e600a2ce59eb8f36f))
* **worker:** scrub secrets from hook output and state API error fields ([#1045](https://github.com/xrf9268-hue/aiops-platform/issues/1045)) ([5a4003b](https://github.com/xrf9268-hue/aiops-platform/commit/5a4003b9fa91d8758dc2b90e600a2ce59eb8f36f))
* **workflow:** keep transient review gates out of blocked ([#1061](https://github.com/xrf9268-hue/aiops-platform/issues/1061)) ([4a39185](https://github.com/xrf9268-hue/aiops-platform/commit/4a39185dc23cea6538041d837d4bde3210eaeee5))

## [0.1.13](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.12...v0.1.13) (2026-07-02)


### Bug Fixes

* **orchestrator:** drain in-flight agent workers on shutdown ([#1044](https://github.com/xrf9268-hue/aiops-platform/issues/1044)) ([8956638](https://github.com/xrf9268-hue/aiops-platform/commit/895663811d484ad978dbddd668eaed07b36f9202))
* **runner:** answer server-&gt;client requests interleaved with pending RPCs ([#1046](https://github.com/xrf9268-hue/aiops-platform/issues/1046)) ([9aa96e5](https://github.com/xrf9268-hue/aiops-platform/commit/9aa96e507549cd5ce8b92cf49627db5b7bdf0397))
* **workflow:** remove rework count budget ([#1051](https://github.com/xrf9268-hue/aiops-platform/issues/1051)) ([c1dd32a](https://github.com/xrf9268-hue/aiops-platform/commit/c1dd32a6ca7a3e26353cdb229157a53789628163))

## [0.1.12](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.11...v0.1.12) (2026-07-02)


### Bug Fixes

* **github:** harden unattended workflow gates ([#1028](https://github.com/xrf9268-hue/aiops-platform/issues/1028)) ([3811dc0](https://github.com/xrf9268-hue/aiops-platform/commit/3811dc0dec2d204fa94308d19ef8c4a563084777))

## [0.1.11](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.10...v0.1.11) (2026-06-29)


### Bug Fixes

* prevent dashboard running table overflow ([#1019](https://github.com/xrf9268-hue/aiops-platform/issues/1019)) ([039ef7d](https://github.com/xrf9268-hue/aiops-platform/commit/039ef7d7918cd3543772c64d697a8ef52b83ad6c))

## [0.1.10](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.9...v0.1.10) (2026-06-26)


### Features

* **e2e:** add Crowd Runner milestone freeze ([#1001](https://github.com/xrf9268-hue/aiops-platform/issues/1001)) ([bc63631](https://github.com/xrf9268-hue/aiops-platform/commit/bc636310cb0ba9e3db6298a295559a9514aa5d8c))
* **observability:** classify active success without handoff ([a5ab303](https://github.com/xrf9268-hue/aiops-platform/commit/a5ab3038624b04b97723a492767d06039eb0e723)), closes [#988](https://github.com/xrf9268-hue/aiops-platform/issues/988)


### Bug Fixes

* **ci:** disable cancel-in-progress on required status check workflows ([#1005](https://github.com/xrf9268-hue/aiops-platform/issues/1005)) ([0915b80](https://github.com/xrf9268-hue/aiops-platform/commit/0915b8014ed841fe90fecc7fc7fc54615d379e17))
* **runner:** pin codex app-server multi-agent mode ([#999](https://github.com/xrf9268-hue/aiops-platform/issues/999)) ([58e52c2](https://github.com/xrf9268-hue/aiops-platform/commit/58e52c22083603859b1f5fb9857de31d1f4d082d))
* **runner:** surface app-server startup retry phase ([8baacc6](https://github.com/xrf9268-hue/aiops-platform/commit/8baacc6b4d09c7f1e9be42c497ae6d74c0186bbf))
* **workflow:** claim GitHub dogfood PR before long gates ([#995](https://github.com/xrf9268-hue/aiops-platform/issues/995)) ([548b00b](https://github.com/xrf9268-hue/aiops-platform/commit/548b00b908daf280b41f9480b1824750866d663e))

## [0.1.9](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.8...v0.1.9) (2026-06-22)


### Features

* **dashboard:** expose per-run workflow profile on /api/v1/state ([#984](https://github.com/xrf9268-hue/aiops-platform/issues/984)) ([8742237](https://github.com/xrf9268-hue/aiops-platform/commit/874223755aa181bebf8affdec0e4fc3296c6dc3d)), closes [#983](https://github.com/xrf9268-hue/aiops-platform/issues/983)
* **dashboard:** show active agent model and runtime profile ([#982](https://github.com/xrf9268-hue/aiops-platform/issues/982)) ([809c583](https://github.com/xrf9268-hue/aiops-platform/commit/809c583f9b153380b4dc6ba322e26d4baad85e6d))


### Bug Fixes

* resolve worktree base to a fully-qualified ref to survive mirror ref pollution ([#979](https://github.com/xrf9268-hue/aiops-platform/issues/979)) ([4f58533](https://github.com/xrf9268-hue/aiops-platform/commit/4f585337105cb0f76b50ac431994f7643245215a)), closes [#976](https://github.com/xrf9268-hue/aiops-platform/issues/976)
* surface local git op stderr in worker dispatch errors ([#981](https://github.com/xrf9268-hue/aiops-platform/issues/981)) ([0765cd3](https://github.com/xrf9268-hue/aiops-platform/commit/0765cd36846c4c13299bec598d51ac9b9d61ef06)), closes [#978](https://github.com/xrf9268-hue/aiops-platform/issues/978)

## [0.1.8](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.7...v0.1.8) (2026-06-21)


### Bug Fixes

* **runner:** wire claude PROMPT.md via cmd.Stdin ([#972](https://github.com/xrf9268-hue/aiops-platform/issues/972)) ([62424fc](https://github.com/xrf9268-hue/aiops-platform/commit/62424fcb7e127037147e6287d33939124bb734c2))

## [0.1.7](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.6...v0.1.7) (2026-06-20)


### Features

* **harness:** add durable redacted trace evidence manifest ([#957](https://github.com/xrf9268-hue/aiops-platform/issues/957)) ([2676bed](https://github.com/xrf9268-hue/aiops-platform/commit/2676bed291dc87221c2a56228791f8844fe8c2ee))
* **harness:** add trace advisory evaluator candidates ([#950](https://github.com/xrf9268-hue/aiops-platform/issues/950)) ([12c6abb](https://github.com/xrf9268-hue/aiops-platform/commit/12c6abbf585879a68a5d80ecf59d29f725aaa57e))
* **harness:** close evaluator-result ratchet ([#959](https://github.com/xrf9268-hue/aiops-platform/issues/959)) ([6a34937](https://github.com/xrf9268-hue/aiops-platform/commit/6a34937cd35650c0f622401696c01767b114b318))
* **harness:** document trace proposal follow-through ([#949](https://github.com/xrf9268-hue/aiops-platform/issues/949)) ([023fd10](https://github.com/xrf9268-hue/aiops-platform/commit/023fd10886f785e3cfabc9c365ba7baaf17b9b24))
* **harness:** recover per-class affected ids for capped manifest runs ([#960](https://github.com/xrf9268-hue/aiops-platform/issues/960)) ([6fbd0b9](https://github.com/xrf9268-hue/aiops-platform/commit/6fbd0b9d429e40f6fdd6c2d292d940977506326d))
* render trace harness proposals ([#948](https://github.com/xrf9268-hue/aiops-platform/issues/948)) ([4ab90b3](https://github.com/xrf9268-hue/aiops-platform/commit/4ab90b3daad8731e969bc2e7cd7868c5b8d5a7cd))
* **trace:** generate harness reports from worker logs ([#942](https://github.com/xrf9268-hue/aiops-platform/issues/942)) ([9287eb3](https://github.com/xrf9268-hue/aiops-platform/commit/9287eb30dfbf32cda0657834e25aa1306445725f))


### Bug Fixes

* **gitea:** close terminal issues through label tool ([#965](https://github.com/xrf9268-hue/aiops-platform/issues/965)) ([45f3a14](https://github.com/xrf9268-hue/aiops-platform/commit/45f3a14c46925e74864295eb685b395219ff69c4))
* **harness:** dedupe byte-identical trace inputs to stop omitted inflation ([#962](https://github.com/xrf9268-hue/aiops-platform/issues/962)) ([f3d660e](https://github.com/xrf9268-hue/aiops-platform/commit/f3d660e94e3c23291f364698887806d567393432))
* **workflow:** tighten web todo rework convergence ([#966](https://github.com/xrf9268-hue/aiops-platform/issues/966)) ([81e349c](https://github.com/xrf9268-hue/aiops-platform/commit/81e349c8841108637be2d980f75992722f79b758))

## [0.1.6](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.5...v0.1.6) (2026-06-17)


### Bug Fixes

* harden env passthrough boundaries ([d861451](https://github.com/xrf9268-hue/aiops-platform/commit/d86145138934887c62adc1af81d956a574af42f7)), closes [#910](https://github.com/xrf9268-hue/aiops-platform/issues/910)

## [0.1.5](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.4...v0.1.5) (2026-06-16)


### Bug Fixes

* **tooling:** re-check NOT-CONFIRMED head for a late Codex review ([#903](https://github.com/xrf9268-hue/aiops-platform/issues/903)) ([d0ff3af](https://github.com/xrf9268-hue/aiops-platform/commit/d0ff3afac1e4cf15ef22427491d6ae65552273a4)), closes [#894](https://github.com/xrf9268-hue/aiops-platform/issues/894)

## [0.1.4](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.3...v0.1.4) (2026-06-15)


### Features

* **gitea:** add merging state label ([#880](https://github.com/xrf9268-hue/aiops-platform/issues/880)) ([adb4587](https://github.com/xrf9268-hue/aiops-platform/commit/adb458741ba72cc70d74377a27d148603052853b))


### Bug Fixes

* **linear:** honor pagination max pages ([#876](https://github.com/xrf9268-hue/aiops-platform/issues/876)) ([3635870](https://github.com/xrf9268-hue/aiops-platform/commit/3635870c4232fa3e7b26a9a57777c30d45e94e3c))
* **tooling:** detect Codex completion by stable id + head-bound review object ([#879](https://github.com/xrf9268-hue/aiops-platform/issues/879)) ([e665974](https://github.com/xrf9268-hue/aiops-platform/commit/e665974f35d1dec416d90d863ab5b42d56c0f572))
* **workspace:** guard live worktree reclaim ([#881](https://github.com/xrf9268-hue/aiops-platform/issues/881)) ([cc97284](https://github.com/xrf9268-hue/aiops-platform/commit/cc972848e5663c4f8c607a97930a99853150f581))
* **workspace:** guard worktree cleanup ([#877](https://github.com/xrf9268-hue/aiops-platform/issues/877)) ([d3f610b](https://github.com/xrf9268-hue/aiops-platform/commit/d3f610b7ca66526a25f6ca55722bf3a3bd1f6d85))
* **workspace:** reclaim stale foreign-root worktree after workspace.root change ([#854](https://github.com/xrf9268-hue/aiops-platform/issues/854)) ([#869](https://github.com/xrf9268-hue/aiops-platform/issues/869)) ([db99fb5](https://github.com/xrf9268-hue/aiops-platform/commit/db99fb5d68afa5310f076a422bcb3e5139682d0c))

## [0.1.3](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.2...v0.1.3) (2026-06-14)


### Features

* **dashboard:** worker-status version chip + favicon (Symphony #90) ([#834](https://github.com/xrf9268-hue/aiops-platform/issues/834)) ([661cf47](https://github.com/xrf9268-hue/aiops-platform/commit/661cf4714ef223fbdbf2366176b905d829e8bc69)), closes [#833](https://github.com/xrf9268-hue/aiops-platform/issues/833)
* **cmd:** version observability (--version, ldflags stamping, /api/v1/state field, --help subcommands) ([#828](https://github.com/xrf9268-hue/aiops-platform/issues/828)) ([19e3c8f](https://github.com/xrf9268-hue/aiops-platform/commit/19e3c8fbe9752cbd477d8961fd12912670f87b9c)), closes [#796](https://github.com/xrf9268-hue/aiops-platform/issues/796)
* **doctor:** preflight Gitea/GitHub trackers by driving the worker's own clients ([#801](https://github.com/xrf9268-hue/aiops-platform/issues/801)) ([072a8de](https://github.com/xrf9268-hue/aiops-platform/commit/072a8deafdbed96e6a31d87340fec5e0920ba245)), closes [#781](https://github.com/xrf9268-hue/aiops-platform/issues/781)


### Bug Fixes

* add panic recovery to the three unguarded goroutines; fix two letter-of-rule violations ([#818](https://github.com/xrf9268-hue/aiops-platform/issues/818)) ([28a69ee](https://github.com/xrf9268-hue/aiops-platform/commit/28a69eecaf44f4e249b96e3e2ba898c56e6f6d47)), closes [#790](https://github.com/xrf9268-hue/aiops-platform/issues/790)

## [0.1.2](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.1...v0.1.2) (2026-06-12)


### Bug Fixes

* **runner,doctor:** drain remaining non-2xx Linear bodies and delete the dead Gitea PR helpers ([#779](https://github.com/xrf9268-hue/aiops-platform/issues/779)) ([027b06e](https://github.com/xrf9268-hue/aiops-platform/commit/027b06e2e2ed1821dea0711c6731f28d42566787)), closes [#771](https://github.com/xrf9268-hue/aiops-platform/issues/771)
* **tracker:** classify HTTP 429 as a typed rate-limited error with Retry-After ([#768](https://github.com/xrf9268-hue/aiops-platform/issues/768)) ([919d353](https://github.com/xrf9268-hue/aiops-platform/commit/919d35351ac09381e2a86fe09ae45ebcbcdbabd4))
* **tracker:** drain non-2xx response bodies so keep-alive connections are reused ([#772](https://github.com/xrf9268-hue/aiops-platform/issues/772)) ([40fa356](https://github.com/xrf9268-hue/aiops-platform/commit/40fa35688377fa08d1a436300b362a791ca858f9)), closes [#762](https://github.com/xrf9268-hue/aiops-platform/issues/762)
* **workspace:** bound every git subprocess with a per-operation timeout ([#764](https://github.com/xrf9268-hue/aiops-platform/issues/764)) ([4b0d276](https://github.com/xrf9268-hue/aiops-platform/commit/4b0d276ba3bf0a53dafcc94c09c8e30c59900ad5))
* **workspace:** heal legacy partial mirrors and reap staging dirs on failed clones ([#774](https://github.com/xrf9268-hue/aiops-platform/issues/774)) ([c45b323](https://github.com/xrf9268-hue/aiops-platform/commit/c45b323ef503af67551baaa3909900281d193415)), closes [#765](https://github.com/xrf9268-hue/aiops-platform/issues/765)

## [0.1.1](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.0...v0.1.1) (2026-06-11)


### Bug Fixes

* map Gitea aiops/todo to SPEC state "Todo" so the blocker gate fires ([#743](https://github.com/xrf9268-hue/aiops-platform/issues/743)) ([c9a8f25](https://github.com/xrf9268-hue/aiops-platform/commit/c9a8f25154feb50c7054f983c1ebbdfe5ea4d900)), closes [#739](https://github.com/xrf9268-hue/aiops-platform/issues/739)
* mirror agent-handoff mutation audit onto gitea_issue_labels ([#748](https://github.com/xrf9268-hue/aiops-platform/issues/748)) ([#751](https://github.com/xrf9268-hue/aiops-platform/issues/751)) ([0472c28](https://github.com/xrf9268-hue/aiops-platform/commit/0472c28e2d86bf74ddfce2baa691d8317005a07b))
* post [@codex](https://github.com/codex) review triggers with a workspace-bound identity ([#747](https://github.com/xrf9268-hue/aiops-platform/issues/747)) ([a6f9e61](https://github.com/xrf9268-hue/aiops-platform/commit/a6f9e619cd76b21e590473364fcac9cdf1131d2b)), closes [#746](https://github.com/xrf9268-hue/aiops-platform/issues/746)
* re-check Todo blockers in dispatch revalidation ([#750](https://github.com/xrf9268-hue/aiops-platform/issues/750)) ([#752](https://github.com/xrf9268-hue/aiops-platform/issues/752)) ([3c59890](https://github.com/xrf9268-hue/aiops-platform/commit/3c59890bf88d3ad690fb544664d3a03e5d4c25cf))
* revalidate dispatch candidates against tracker state before spawning ([#749](https://github.com/xrf9268-hue/aiops-platform/issues/749)) ([e5c5e41](https://github.com/xrf9268-hue/aiops-platform/commit/e5c5e41e29ca066631a74f51bf9a4eaf7aa8ac7f)), closes [#740](https://github.com/xrf9268-hue/aiops-platform/issues/740)

## 0.1.0 (2026-06-10)


### Features

* automate release with release-please Release PR + tag cut ([#734](https://github.com/xrf9268-hue/aiops-platform/issues/734)) ([#735](https://github.com/xrf9268-hue/aiops-platform/issues/735)) ([87f1392](https://github.com/xrf9268-hue/aiops-platform/commit/87f13928d81fe5bdec4ded220706f24921de2054))
