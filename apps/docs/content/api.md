---
title: API
order: 20
---

# OGA9 API

Base URL: `/api/v1` on the API service (dev: `http://localhost:8080`).

## Conventions

- **Auth**: `Authorization: Bearer <token>` — JWT sessions keyed to a
  server-side JTI, revocable per session.
- **Success**: `{"data": ...}` — errors:
  `{"error": {"code": "snake_case", "message": "...", "status": 4xx}}`.
- **Idempotency**: financial endpoints require an `Idempotency-Key` header.
- **Rate limits**: 429 + `Retry-After` on abuse tiers.

## Identity

`POST /auth/register` · `POST /auth/login` (+ optional `mfa_code`) ·
`POST /auth/verify/{token}` · `POST /auth/verify/resend` ·
`POST /auth/password/forgot` · `POST /auth/password/reset` ·
`GET /auth/oauth/{provider}/start` · `POST /auth/mfa/setup|confirm|disable` ·
`GET /me` · `GET /me/sessions` · `DELETE /me/sessions/{id}` ·
`POST /auth/logout`
