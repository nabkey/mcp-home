# Changelog

## [1.8.0](https://github.com/nabkey/mcp-home/compare/v1.7.0...v1.8.0) (2026-06-27)


### Features

* **esphome:** surface build/flash logs in get_esphome_job via follow_job ([#31](https://github.com/nabkey/mcp-home/issues/31)) ([292e667](https://github.com/nabkey/mcp-home/commit/292e6670e0808016e0ac74a3461e699cde1a49e9))

## [1.7.0](https://github.com/nabkey/mcp-home/compare/v1.6.1...v1.7.0) (2026-06-27)


### Features

* **esphome:** async job-based compile/upload to survive long builds ([#29](https://github.com/nabkey/mcp-home/issues/29)) ([b411948](https://github.com/nabkey/mcp-home/commit/b411948638af65e61a87bc6e0220e1b982ddf521))

## [1.6.1](https://github.com/nabkey/mcp-home/compare/v1.6.0...v1.6.1) (2026-06-27)


### Bug Fixes

* **esphome:** rewrite client for the new Device Builder /ws API ([#27](https://github.com/nabkey/mcp-home/issues/27)) ([43b7469](https://github.com/nabkey/mcp-home/commit/43b746948d5a9e4383fa95a71d5b83c2887d76f9))

## [1.6.0](https://github.com/nabkey/mcp-home/compare/v1.5.0...v1.6.0) (2026-06-26)


### Features

* add ESPHome dashboard integration group ([#25](https://github.com/nabkey/mcp-home/issues/25)) ([b839eae](https://github.com/nabkey/mcp-home/commit/b839eae67c0bba7ea6614a988dcfff25bbf89b89))

## [1.5.0](https://github.com/nabkey/mcp-home/compare/v1.4.1...v1.5.0) (2026-06-10)


### Features

* service deny list, per-user audit logging, tool annotations, --log-level ([b0c4f81](https://github.com/nabkey/mcp-home/commit/b0c4f8141ed89f81d164a6a8a162b3a11324caa9))


### Bug Fixes

* **deps:** update Go to 1.26.4 for stdlib vulnerability fixes ([4605e8f](https://github.com/nabkey/mcp-home/commit/4605e8f719d17d0861235694fefe4beb56150fd5))

## [1.4.1](https://github.com/nabkey/mcp-home/compare/v1.4.0...v1.4.1) (2026-06-10)


### Bug Fixes

* address lint findings across the codebase ([13b5c80](https://github.com/nabkey/mcp-home/commit/13b5c80044488ded37b7917410ee34c1fc1e9269))
* preserve base URL path prefixes and rate-limit Access cert refetches ([41f6ba7](https://github.com/nabkey/mcp-home/commit/41f6ba7d17be45194708fbaa8d14917b4b69a40b))
* unbreak SSE streaming, shut down HTTP server on tunnel error, report real version ([b3500a5](https://github.com/nabkey/mcp-home/commit/b3500a51187a459a1d1e52c64179a6ac4bd07c77))

## [1.4.0](https://github.com/nabkey/mcp-home/compare/v1.3.1...v1.4.0) (2026-06-04)


### Features

* **hass:** add execute_script tool for ad-hoc action sequences ([1d5875f](https://github.com/nabkey/mcp-home/commit/1d5875fe842d720697fae48a56e6e136c1c14ed2))
* **hass:** add get_diagnostics tool ([1fa2288](https://github.com/nabkey/mcp-home/commit/1fa22885e4257f187c8791544e8d63bfe8fa7ad7))
* **hass:** add Lovelace dashboard management tools ([313bd5c](https://github.com/nabkey/mcp-home/commit/313bd5c43520b3ee6a0231a9beff06a61798b18a))
* **hass:** add registry write tool (manage_registry) ([2a1554c](https://github.com/nabkey/mcp-home/commit/2a1554c87e4f1f16a1706508324dae556c6e869a))

## [1.3.1](https://github.com/nabkey/mcp-home/compare/v1.3.0...v1.3.1) (2026-05-07)


### Dependencies

* bump publish workflow Actions to Node 24 majors ([b8702d5](https://github.com/nabkey/mcp-home/commit/b8702d509337a0fe080aa97e47190665a85c9fbe))

## [1.3.0](https://github.com/nabkey/mcp-home/compare/v1.2.0...v1.3.0) (2026-05-02)


### Features

* **hass:** add registry, history, template, scenes, stats, calendar tools ([bd2acfc](https://github.com/nabkey/mcp-home/commit/bd2acfca8fad0f55bb95dd0be86facbece7a89f2))

## [1.2.0](https://github.com/nabkey/mcp-home/compare/v1.1.0...v1.2.0) (2026-05-02)


### Features

* **hass:** add manage_scripts tool for HA script CRUD ([1102562](https://github.com/nabkey/mcp-home/commit/1102562202e67b9062951cbe92e41dfd581dd291))

## [1.1.0](https://github.com/nabkey/mcp-home/compare/v1.0.0...v1.1.0) (2026-04-27)


### Features

* **hass:** add CRUD support for Home Assistant helpers ([9d091ad](https://github.com/nabkey/mcp-home/commit/9d091ada017a07763d8d636abb5b5898e68301e5))

## 1.0.0 (2026-04-17)


### Features

* add container publishing and release-please automation ([6596188](https://github.com/nabkey/mcp-home/commit/65961880c7149dd2ab30669dd178e9589dd564b0))
