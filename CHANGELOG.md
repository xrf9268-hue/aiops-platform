# Changelog

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
