# Changelog

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
