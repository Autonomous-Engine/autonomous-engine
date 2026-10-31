# Product Requirements Document

## Overview
This document outlines the requirements for fixing a specific bug related to the Mapforge basemap API key configuration in the Station Finder application. The goal is to ensure that the API key is dynamically fetched from the environment configuration and used correctly in the tile URLs, while also providing clear feedback to users and developers when the key is missing.

## Problem Statement
The Station Finder application's map functionality is currently broken due to missing API keys for the Mapforge basemap service. The tile URLs for the night basemap and labels do not include the required API key, resulting in an error message stating "api key required mapforge.example/keys". This issue needs to be resolved by ensuring that the API key is dynamically fetched from the environment and correctly appended to the tile URLs.

## Goals
1. **Dynamic API Key Configuration**: The Mapforge API key should be fetched from the environment configuration and used in the tile URLs.
2. **User Feedback**: When the API key is missing, the UI should display a clear notice indicating that the basemap is unavailable and provide guidance on how to configure the API key.
3. **Server Logging**: The server should log a warning at startup if the Mapforge API key is not configured.
4. **Documentation Update**: The README, .env.example, docker-compose.yml, and configuration documentation should be updated to reflect the requirement for a Mapforge API key.
5. **Unit Testing**: A unit test should be added to verify the functionality of the new `/api/client-config` endpoint.

## Non-Goals
- The station feed importer should not be modified.
- No realistic-looking placeholder API keys should be invented or guessed.
- The existing functionality of the application, other than the map, should remain unchanged.

## User Stories
1. **As a developer**, I want to configure the Mapforge API key in the environment so that the basemap displays correctly.
2. **As a user**, I want to see a clear notice when the basemap is unavailable due to a missing API key so that I can understand the issue.
3. **As a developer**, I want to see a warning in the server logs when the Mapforge API key is not configured so that I can address the issue promptly.

## Acceptance Criteria
1. **Dynamic API Key Configuration**:
   - The `mapforgeApiKey` should be added to `src/config/env.ts` with a default value of `process.env.MAPFORGE_API_KEY || ''`.
   - A new GET `/api/client-config` route should be added to `src/server/index.ts` that returns a JSON object with the `mapforgeApiKey` and a `basemapConfigured` boolean indicating whether the key is set.
   - Both `public/index.html` and `landing/index.html` should fetch the `/api/client-config` endpoint before creating the tile layers and append `?api_key=<key>` to the Mapforge tile URLs only if a key is configured.

2. **User Feedback**:
   - Both `public/index.html` and `landing/index.html` should display a visible, non-blocking notice "Basemap unavailable: MAPFORGE_API_KEY is not configured. See README (Configuration)." when the API key is missing.
   - The "no API keys" claim in `landing/index.html` should be removed or corrected.

3. **Server Logging**:
   - The server should log a warning at startup if the `MAPFORGE_API_KEY` environment variable is empty.

4. **Documentation Update**:
   - README.md, .env.example, docker-compose.yml, and docs/configuration.md should document the requirement for a Mapforge API key, including instructions on how to obtain one from https://mapforge.example/keys.
   - The pull request description should explicitly state that the repository owner must obtain a Mapforge API key and set `MAPFORGE_API_KEY` in the deployment environment for the basemap error to go away.

5. **Unit Testing**:
   - A vitest unit test should be added to `tests/unit/server.test.ts` to verify that the `/api/client-config` endpoint returns the correct `mapforgeApiKey` and `basemapConfigured` values based on the environment configuration.

6. **Test Suite**:
   - `npm test` should pass without any new failures.

## Open Questions
- None identified. The requirements are clear and do not require further clarification from the business owner.
