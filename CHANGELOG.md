# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog. This update retains the v1 module path;
API and storage-format changes are listed explicitly below.

## [v1.1.0] - 2026-11-04

### Breaking changes
- Optional configuration fields use pointers, shutdown returns errors, and custom client interfaces use `I` prefixes.
- Lease ownership uses a length-prefixed format; old and new leases must not share active keys.
- Rate-limit inputs must fit supported precision and duration ranges.

### Validation
- Added offline protocol tests, executable examples, and real Redis 6.2/7.4/8 integration coverage.
- Added formatting, race, vet, build, module, and vulnerability CI checks.

## Previous unreleased notes

### Added
- Redis client manager with `Init`, `Get`, `Close`, and `CloseAll`.
- Distributed lock manager with single-key and multi-key atomic lock operations.
- Lua-based rate limiter supporting:
  - single-rule checks (`Allow`, `AllowN`, `AllowAtMost`)
  - multi-rule atomic checks (`AllowNRules`, `AllowAtMostRules`)
- GitHub CI workflow for format/build/test checks.
- Project documentation (`README`, contribution and security policies).

### Changed
- Removed implicit Redis key prefix behavior in limiter (keys are fully caller-controlled).

