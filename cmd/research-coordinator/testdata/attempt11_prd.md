# Product Requirements Document

## Overview
This document outlines the requirements for fixing a bug in the Station Finder application where the Mapforge tile error "api key required mapforge.example/keys" is displayed instead of the night basemap. The fix involves ensuring that the Mapforge API key is dynamically retrieved from the configuration and used in the tile URLs.

## Problem Statement
The Station Finder map displays an error message "api key required mapforge.example/keys" because the Mapforge basemap tile URLs do not include an API key. This issue affects the user experience by preventing the night basemap from rendering correctly.

## Goals
1. **Dynamic API Key Retrieval**: Ensure that the Mapforge API key is retrieved from the configuration and appended to the tile URLs dynamically.
2. **User Notification**: Provide a clear, non-blocking notice to users when the Mapforge API key is not configured.
3. **Server Logging**: Log a warning at server startup if the Mapforge API key is not set.
4. **Documentation Update**: Update relevant documentation to inform users about obtaining and configuring the Mapforge API key.
5. **Testing**: Add a unit test to verify the behavior of the `/api/client-config` endpoint.

## Non-Goals
- Inventing, guessing, or hard-coding a Mapforge API key.
- Modifying the station feed importer.
- Changing the behavior of the application if the API key is not provided, other than displaying a notice.

## User Stories
1. **As a user**, I want to see the night basemap without errors, **so that** I can use the application as intended.
2. **As a developer**, I want the Mapforge API key to be configurable, **so that** I can deploy the application in different environments without hard-coding sensitive information.
3. **As an administrator**, I want to be notified if the Mapforge API key is missing, **so that** I can configure it appropriately.

## Acceptance Criteria
1. **API Key Configuration**:
   - Add `mapforgeApiKey: process.env.MAPFORGE_API_KEY || ''` to `src/config/env.ts`.
   - Implement a GET `/api/client-config` route in `src/server/index.ts` that returns a JSON object with `mapforgeApiKey` and `basemapConfigured`.
   - Both `public/index.html` and `landing/index.html` should fetch the configuration before creating the tile layers and append `?api_key=<key>` to the Mapforge tile URLs if a key is configured.

2. **User Notification**:
   - Both HTML pages should display a visible, non-blocking notice "Basemap unavailable: MAPFORGE_API_KEY is not configured. See README (Configuration)." when the key is missing.
   - The server should log a warning at startup if the Mapforge API key is empty.

3. **Documentation Update**:
   - Update `README.md`, `.env.example`, `docker-compose.yml`, and `docs/configuration.md` to document the MAPFORGE_API_KEY configuration.
   - Correct the "no API keys" claim in `landing/index.html`.

4. **Unit Testing**:
   - Add a vitest unit test for the `/api/client-config` endpoint that verifies it returns the correct configuration based on the presence or absence of the Mapforge API key.

5. **Pull Request Description**:
   - The pull request description must explicitly state that the repository owner must obtain a Mapforge API key and set MAPFORGE_API_KEY in the deployment environment (.env / docker-compose) for the basemap error to go away.

6. **Test Suite**:
   - Ensure that `npm test` passes without any new failures.

## Open Questions
- None identified. The requirements are clear and do not require additional business owner input.

### Review Findings:
1. **Completeness**: All required sections are present and substantive.
2. **Clarity**: The acceptance criteria are testable and unambiguous.
3. **Consistency**: There are no contradictions between sections.

The PRD is unchanged and meets all the required criteria.
