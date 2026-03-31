# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog and this project follows Semantic Versioning.

## [Unreleased]

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

