# Security Policy

## Supported Versions

Until Mamari reaches 1.0, only the latest tagged release receives security
fixes.

## Reporting a Vulnerability

Use GitHub's private vulnerability reporting from the repository's
**Security** tab. For an internal deployment where that feature is unavailable,
contact the repository maintainer directly and avoid including sensitive source
code, credentials, or exploit details in a public issue.

Please include:

- the affected Mamari version and operating system;
- the impact and required preconditions;
- a minimal reproduction that does not contain proprietary code;
- any suggested mitigation.

Mamari indexes local source and runs as a local stdio MCP process. Reports about
unexpected network access, source disclosure, unsafe file writes, archive or
installer integrity, and MCP command execution are treated as security issues.
