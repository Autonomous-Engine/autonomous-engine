# Product Requirements Document

## Overview
This document outlines the requirements for fixing a bug in the sample-app application where the TileHost tile error "api key required tiles.example.com/keys" is displayed instead of the dark basemap. The fix involves ensuring that the TileHost API key is dynamically fetched from the configuration and appended to the tile URLs if available.

## Problem Statement
The sample-app map displays an error message instead of the dark basemap due to the absence of an API key in the TileHost tile URLs. The current implementation does not dynamically fetch the API key from the configuration, leading to the error message when the key is not hard-coded.

## Goals
1. **Dynamic API Key Fetching**: Implement a mechanism to fetch the TileHost API key from the configuration and append it to the tile URLs if available.
2. **User Notification**: Display a clear, non-blocking notice on both HTML pages when the TileHost API key is missing.
3. **Server Logging**: Log a warning at server startup if the TileHost API key is not configured.
4. **Documentation Update**: Update relevant documentation to inform users about the need to configure the TileHost API key.
5. **Testing**: Add a unit test to verify the behavior of the `/api/client-config` endpoint.

## Non-Goals
- Hard-coding or inventing a TileHost API key.
- Modifying the data import pipeline.
- Fully resolving the error without obtaining a TileHost API key.

## User Stories
1. **As a user, I want to see the dark basemap without errors, so that I can use the application as intended.**
2. **As a developer, I want to configure the TileHost API key through environment variables, so that I can easily manage API keys across different environments.**
3. **As a system administrator, I want to receive a warning if the TileHost API key is not configured, so that I can take appropriate action.**

## Acceptance Criteria
1. **API Key Configuration**:
   - Add `tilesApiKey: process.env.TILES_API_KEY || ''` to `src/config/env.ts`.
   - Implement a GET `/api/client-config` route in `src/server/index.ts` that returns JSON with `{"tilesApiKey": string, "basemapConfigured": boolean}`.
   - Both HTML pages fetch the client configuration before creating the tile layers and append `?api_key=<key>` to the TileHost tile URLs only when a key is configured.

2. **User Notification**:
   - Both `public/index.html` and `landing/index.html` display a visible, non-blocking notice "Basemap unavailable: TILES_API_KEY is not configured. See README (Configuration)." when the key is missing.
   - Update the meta description in `landing/index.html` to remove the incorrect claim "Free to use, no sign-up required."

3. **Server Logging**:
   - The server logs a warning at startup when `TILES_API_KEY` is empty.

4. **Documentation Update**:
   - Update `README.md`, `.env.example`, `docker-compose.yml`, and `docs/configuration.md` to document the need for `TILES_API_KEY` and provide instructions on obtaining it.

5. **Unit Testing**:
   - Add a vitest unit test for the `/api/client-config` endpoint that verifies it returns the correct configuration based on the presence or absence of the `TILES_API_KEY`.

6. **Pull Request Description**:
   - The pull request description explicitly states that the repository owner must obtain a TileHost API key and set `TILES_API_KEY` in the deployment environment for the basemap error to go away.

7. **Test Suite**:
   - Ensure `npm test` passes without any changes to the existing test suite.

## Open Questions
None. All requirements are clearly defined and do not require further clarification from the business owner.