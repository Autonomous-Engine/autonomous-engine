# Recorded-decomposition corpus (R9 P3)

Planner runs, replayed offline by `TestRecordedDecompositionCorpus`
(`corpus_test.go`) through `planWithPolicy` and the real `decomposeIntoTasks`
against a local fake provider. No live model is called.

The cases are synthetic fixtures modelled on early live runs: the briefs,
prompts and model responses keep the structure and the defects of those
runs (criteria ids, files, checks, the tasks the model got wrong), but the
application is a made-up "sample-app", a small web app whose two map pages
load tiles from a fictional provider ("TileHost") that requires an API key
(`TILES_API_KEY`).

| case | work item | live outcome |
|---|---|---|
| attempt5 | 72078860-6f2d-428f-a138-cf85ee18c7f3 | blocked: orphan AC-7 after repair (docs task dropped) |
| attempt6 | e69051fa-99f4-431b-80ca-9d1bf6bf9060 | planned after one repair round |
| attempt7 | 1ceb083b-ca2e-4a25-b20a-d157de505b26 | blocked: derived AC-9 reuses a brief id |
| attempt7b | 1ceb083b-… (Retry) | blocked: derived AC-9 reuses a brief id |
| attempt8 | 1ceb083b-… (Retry) | blocked: orphan AC-6 after repair |
| attempt9 | 1ceb083b-… (Retry) | planned after one repair round |
| attempt13 | 63f733e0-de21-40ad-b652-2cff5190ee40 | blocked: [unknown_criterion] AC-11 after repair (task 9 declared derived AC-11, cited nothing; the repair cited it without its definition) |

Each case directory holds:

- `brief.json`: the `technical_brief` artifact's `content` (the planner
  reads the newest one that is not `plan-validation`).
- `response_N.txt`: the Nth `decompose_tasks` call's `response_content`.
- `prompt_N.json`: that call's `prompt_messages` (provenance only).
- `source.json`: the work item, the artifact id, and per response its
  `inference_log_id`, model, and `answers`: the prompt it answered.
  `decompose` is the plain decomposition prompt, `correction` the
  re-prompt after an unparseable or strictly rejected answer ("previous
  response was rejected"), and `repair` the plan-validation repair round
  ("rejected by the plan validator").

The fake answers each request with the next response of the request's role.
If the current planner asks for something the run never recorded, the case
ends `unrecorded:<role>` and asserts what the planner asked instead.

## Adding a case

1. Find the run's rows (read-only):

   ```sh
   docker exec postgres psql -U ae_user -d autonomous_engine -At -c \
     "SELECT id, created_at FROM inference_logs WHERE work_item_id='<work item>' AND intent_type='decompose_tasks' ORDER BY created_at"
   docker exec postgres psql -U ae_user -d autonomous_engine -At -c \
     "SELECT id, created_at FROM work_item_artifacts WHERE work_item_id='<work item>' AND artifact_type='technical_brief' AND COALESCE(content->>'name','')<>'plan-validation' ORDER BY created_at"
   ```

   A Retry re-synthesises the brief, so pair each planning run's calls with
   the brief written just before them.

2. Export them into `testdata/corpus/<case>/`:

   ```sh
   docker exec postgres psql -U ae_user -d autonomous_engine -At -c \
     "SELECT content FROM work_item_artifacts WHERE id='<artifact>'" > brief.json
   docker exec postgres psql -U ae_user -d autonomous_engine -At -c \
     "SELECT response_content FROM inference_logs WHERE id='<log>'" > response_1.txt
   docker exec postgres psql -U ae_user -d autonomous_engine -At -c \
     "SELECT prompt_messages FROM inference_logs WHERE id='<log>'" > prompt_1.json
   ```

   Check the files for secrets before committing them.

3. Write `source.json` as the other cases do, with each response's role
   (read the prompt: "rejected by the plan validator" is `repair`, "previous
   response was rejected" or "could not be parsed" is `correction`).

4. Add the case to `corpusCases` in `corpus_test.go` with what happened live
   (`recorded`) and what the planner must do now (`want`). Run
   `go test ./cmd/implementation-planner -run TestRecordedDecompositionCorpus -v`;
   the log line per case shows the current outcome.
