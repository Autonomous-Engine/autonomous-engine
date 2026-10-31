package main

// The Acceptance Criteria sections of the PRDs product-agent wrote in the
// live runs (attempts 5, 6 and 7), verbatim apart from the surrounding
// sections being shortened and the application's names being replaced by
// the synthetic sample-app's (Station Finder, Mapforge tiles). Attempt 7's criteria are bullets
// under lettered "###" sub-headings; before R7 the first sub-heading ended
// the section and the coordinator logged acceptance_criteria=0.

const attempt7PRD = "# Product Requirements Document\n\n" +
	"## Goals\n1. **Dynamic API Key Configuration**: Ensure that the Mapforge API key is fetched from the configuration.\n2. **User Notification**: Provide a clear notice.\n\n" +
	"## Acceptance Criteria\n" +
	"### A. Dynamic API Key Configuration\n" +
	"- The `mapforgeApiKey` is added to `src/config/env.ts` as `mapforgeApiKey: process.env.MAPFORGE_API_KEY || ''`.\n" +
	"- A new GET `/api/client-config` route is added to `src/server/index.ts` that returns a JSON object with the `mapforgeApiKey` and a `basemapConfigured` boolean.\n" +
	"- Both `public/index.html` and `landing/index.html` fetch the `/api/client-config` endpoint before creating the tile layers and append `?api_key=<key>` to the Mapforge tile URLs if the key is configured.\n\n" +
	"### B. User Notification and Server Logging\n" +
	"- Both `public/index.html` and `landing/index.html` display a visible, non-blocking notice \"Basemap unavailable: MAPFORGE_API_KEY is not configured. See README (Configuration).\" when the API key is missing.\n" +
	"- The server logs a warning at startup if the `MAPFORGE_API_KEY` environment variable is empty.\n" +
	"- The \"no API keys\" claim in `landing/index.html` is corrected.\n" +
	"- Documentation (`README.md`, `.env.example`, `docker-compose.yml`, `docs/configuration.md`) is updated to inform users about obtaining and setting the Mapforge API key.\n\n" +
	"### C. Unit Testing\n" +
	"- A unit test is added in `tests/unit/server.test.ts` to verify that the `/api/client-config` endpoint returns the correct `mapforgeApiKey` and `basemapConfigured` status based on the presence or absence of the `MAPFORGE_API_KEY` environment variable.\n\n" +
	"## Open Questions\n" +
	"- None. All requirements are clearly defined and do not require additional business owner input.\n\n" +
	"### Review Findings\n" +
	"1. **Completeness**: All required sections are present and substantive.\n" +
	"2. **Clarity**: The acceptance criteria are testable and unambiguous.\n" +
	"3. **Consistency**: There are no contradictions between sections.\n\n" +
	"The PRD is unchanged and complete as reviewed.\n"

const attempt6PRD = "# Product Requirements Document\n\n" +
	"## Acceptance Criteria\n" +
	"1. **Configuration**: `mapforgeApiKey: process.env.MAPFORGE_API_KEY || ''` is added to `src/config/env.ts`.\n" +
	"2. **API Endpoint**: A new GET `/api/client-config` route is added to `src/server/index.ts` that returns JSON with `mapforgeApiKey` and `basemapConfigured`.\n" +
	"3. **Dynamic URL**: Both `public/index.html` and `landing/index.html` fetch the client config before creating tile layers and append `?api_key=<key>` to the Mapforge tile URLs if a key is configured.\n" +
	"4. **User Notice**: Both HTML pages display a visible, non-blocking notice \"Basemap unavailable: MAPFORGE_API_KEY is not configured. See README (Configuration).\" when the key is missing.\n" +
	"5. **Server Logging**: The server logs a warning at startup if `MAPFORGE_API_KEY` is empty.\n" +
	"6. **Documentation**: `README.md`, `.env.example`, `docker-compose.yml`, and `docs/configuration.md` are updated to document `MAPFORGE_API_KEY` and provide instructions for obtaining it.\n" +
	"7. **Unit Test**: A vitest unit test is added to verify that `/api/client-config` returns the correct configuration based on the environment variable.\n" +
	"8. **Pull Request Description**: The pull request description explicitly states that the repository owner must obtain a Mapforge API key and set `MAPFORGE_API_KEY` in the deployment environment for the basemap error to go away.\n" +
	"9. **Tests Pass**: `npm test` must pass without any new failures.\n\n" +
	"## Open Questions\n" +
	"- None identified. The requirements are clear and do not require additional business owner input.\n"

const attempt5PRD = "# Product Requirements Document\n\n" +
	"## Acceptance Criteria\n" +
	"1. **Configuration Management**:\n" +
	"   - Add `mapforgeApiKey: process.env.MAPFORGE_API_KEY || ''` to `src/config/env.ts`.\n" +
	"   - Add a GET `/api/client-config` route in `src/server/index.ts` returning JSON `{\"mapforgeApiKey\": string, \"basemapConfigured\": boolean}`.\n" +
	"   - Both HTML pages fetch `/api/client-config` before creating the tile layers and append `?api_key=<key>` to the Mapforge tile URLs only when a key is configured.\n\n" +
	"2. **User Notification**:\n" +
	"   - Both `public/index.html` and `landing/index.html` show a visible, non-blocking notice \"Basemap unavailable: MAPFORGE_API_KEY is not configured. See README (Configuration).\" when the API key is missing.\n" +
	"   - Update the \"no API keys\" claim in `landing/index.html` to reflect the requirement for obtaining a Mapforge API key.\n\n" +
	"3. **Server Logging**:\n" +
	"   - The server logs a warning at startup when `MAPFORGE_API_KEY` is empty.\n\n" +
	"4. **Documentation Update**:\n" +
	"   - Update `README.md`, `.env.example`, `docker-compose.yml`, and `docs/configuration.md` to document `MAPFORGE_API_KEY` and provide instructions for obtaining and setting it.\n\n" +
	"5. **Testing**:\n" +
	"   - Add a vitest unit test for `/api/client-config`: the test should verify that the endpoint returns the env key when set and returns an empty key and `basemapConfigured=false` when unset.\n\n" +
	"6. **Pull Request Description**:\n" +
	"   - The pull request description must explicitly state that the repository owner must obtain a Mapforge API key and set `MAPFORGE_API_KEY` in the deployment environment for the basemap error to go away.\n\n" +
	"7. **Test Suite**:\n" +
	"   - `npm test` must pass without any issues introduced by the changes.\n"
