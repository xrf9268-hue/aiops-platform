# Changelog

## [0.1.3](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.2...v0.1.3) (2026-06-13)


### Features

* **doctor:** preflight Gitea/GitHub trackers by driving the worker's own clients ([#801](https://github.com/xrf9268-hue/aiops-platform/issues/801)) ([072a8de](https://github.com/xrf9268-hue/aiops-platform/commit/072a8deafdbed96e6a31d87340fec5e0920ba245)), closes [#781](https://github.com/xrf9268-hue/aiops-platform/issues/781)


### Miscellaneous Chores

* archive the [#780](https://github.com/xrf9268-hue/aiops-platform/issues/780)/[#781](https://github.com/xrf9268-hue/aiops-platform/issues/781)/[#782](https://github.com/xrf9268-hue/aiops-platform/issues/782) batch Trellis tasks and record the session journal ([#807](https://github.com/xrf9268-hue/aiops-platform/issues/807)) ([4652f3d](https://github.com/xrf9268-hue/aiops-platform/commit/4652f3dbd8452b80c7b0619c0642412ced3f12d8))
* archive the [#783](https://github.com/xrf9268-hue/aiops-platform/issues/783)/[#784](https://github.com/xrf9268-hue/aiops-platform/issues/784)/[#785](https://github.com/xrf9268-hue/aiops-platform/issues/785) batch Trellis tasks and record the session journal ([#813](https://github.com/xrf9268-hue/aiops-platform/issues/813)) ([987ce8a](https://github.com/xrf9268-hue/aiops-platform/commit/987ce8a21611d4cfaf313002cbb6cdc15baaf498))
* dead-code sweep — drop ReadSensitiveArtifact, workflow.NewError, terminal_update.go, dead tracker/actor APIs ([#788](https://github.com/xrf9268-hue/aiops-platform/issues/788)) ([461ab22](https://github.com/xrf9268-hue/aiops-platform/commit/461ab225a6cd0818cb33dcc863cd39c92cc33247))

## [0.1.2](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.1...v0.1.2) (2026-06-12)


### Bug Fixes

* **runner,doctor:** drain remaining non-2xx Linear bodies and delete the dead Gitea PR helpers ([#779](https://github.com/xrf9268-hue/aiops-platform/issues/779)) ([027b06e](https://github.com/xrf9268-hue/aiops-platform/commit/027b06e2e2ed1821dea0711c6731f28d42566787)), closes [#771](https://github.com/xrf9268-hue/aiops-platform/issues/771)
* **tracker:** classify HTTP 429 as a typed rate-limited error with Retry-After ([#768](https://github.com/xrf9268-hue/aiops-platform/issues/768)) ([919d353](https://github.com/xrf9268-hue/aiops-platform/commit/919d35351ac09381e2a86fe09ae45ebcbcdbabd4))
* **tracker:** drain non-2xx response bodies so keep-alive connections are reused ([#772](https://github.com/xrf9268-hue/aiops-platform/issues/772)) ([40fa356](https://github.com/xrf9268-hue/aiops-platform/commit/40fa35688377fa08d1a436300b362a791ca858f9)), closes [#762](https://github.com/xrf9268-hue/aiops-platform/issues/762)
* **workspace:** bound every git subprocess with a per-operation timeout ([#764](https://github.com/xrf9268-hue/aiops-platform/issues/764)) ([4b0d276](https://github.com/xrf9268-hue/aiops-platform/commit/4b0d276ba3bf0a53dafcc94c09c8e30c59900ad5))
* **workspace:** heal legacy partial mirrors and reap staging dirs on failed clones ([#774](https://github.com/xrf9268-hue/aiops-platform/issues/774)) ([c45b323](https://github.com/xrf9268-hue/aiops-platform/commit/c45b323ef503af67551baaa3909900281d193415)), closes [#765](https://github.com/xrf9268-hue/aiops-platform/issues/765)


### Miscellaneous Chores

* archive the issue-765 Trellis task and record the session journal ([#776](https://github.com/xrf9268-hue/aiops-platform/issues/776)) ([5df7640](https://github.com/xrf9268-hue/aiops-platform/commit/5df764074b80bc4e3c878715578c9d2fec265e0c)), closes [#775](https://github.com/xrf9268-hue/aiops-platform/issues/775)

## [0.1.1](https://github.com/xrf9268-hue/aiops-platform/compare/v0.1.0...v0.1.1) (2026-06-11)


### Bug Fixes

* map Gitea aiops/todo to SPEC state "Todo" so the blocker gate fires ([#743](https://github.com/xrf9268-hue/aiops-platform/issues/743)) ([c9a8f25](https://github.com/xrf9268-hue/aiops-platform/commit/c9a8f25154feb50c7054f983c1ebbdfe5ea4d900)), closes [#739](https://github.com/xrf9268-hue/aiops-platform/issues/739)
* mirror agent-handoff mutation audit onto gitea_issue_labels ([#748](https://github.com/xrf9268-hue/aiops-platform/issues/748)) ([#751](https://github.com/xrf9268-hue/aiops-platform/issues/751)) ([0472c28](https://github.com/xrf9268-hue/aiops-platform/commit/0472c28e2d86bf74ddfce2baa691d8317005a07b))
* post [@codex](https://github.com/codex) review triggers with a workspace-bound identity ([#747](https://github.com/xrf9268-hue/aiops-platform/issues/747)) ([a6f9e61](https://github.com/xrf9268-hue/aiops-platform/commit/a6f9e619cd76b21e590473364fcac9cdf1131d2b)), closes [#746](https://github.com/xrf9268-hue/aiops-platform/issues/746)
* re-check Todo blockers in dispatch revalidation ([#750](https://github.com/xrf9268-hue/aiops-platform/issues/750)) ([#752](https://github.com/xrf9268-hue/aiops-platform/issues/752)) ([3c59890](https://github.com/xrf9268-hue/aiops-platform/commit/3c59890bf88d3ad690fb544664d3a03e5d4c25cf))
* revalidate dispatch candidates against tracker state before spawning ([#749](https://github.com/xrf9268-hue/aiops-platform/issues/749)) ([e5c5e41](https://github.com/xrf9268-hue/aiops-platform/commit/e5c5e41e29ca066631a74f51bf9a4eaf7aa8ac7f)), closes [#740](https://github.com/xrf9268-hue/aiops-platform/issues/740)


### Miscellaneous Chores

* archive automated-release task and record session journal ([#737](https://github.com/xrf9268-hue/aiops-platform/issues/737)) ([ff1c4a8](https://github.com/xrf9268-hue/aiops-platform/commit/ff1c4a82a925c9455ccec0c02d4b0056bc5100fc))
* archive Trellis task journals for the 2026-06-11 issue batch ([#758](https://github.com/xrf9268-hue/aiops-platform/issues/758)) ([58e7809](https://github.com/xrf9268-hue/aiops-platform/commit/58e780953ca5ca0f7b04d946614dd7495c2f2e73))
* record v0.1.0 packaged-binary lifecycle test, archive task and journal ([#742](https://github.com/xrf9268-hue/aiops-platform/issues/742)) ([011341f](https://github.com/xrf9268-hue/aiops-platform/commit/011341fb896312db950fcc15e629f908e9d6f245))

## 0.1.0 (2026-06-10)


### Features

* automate release with release-please Release PR + tag cut ([#734](https://github.com/xrf9268-hue/aiops-platform/issues/734)) ([#735](https://github.com/xrf9268-hue/aiops-platform/issues/735)) ([87f1392](https://github.com/xrf9268-hue/aiops-platform/commit/87f13928d81fe5bdec4ded220706f24921de2054))
