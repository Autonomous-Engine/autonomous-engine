ALTER TABLE inference_logs
    DROP COLUMN IF EXISTS prompt_messages,
    DROP COLUMN IF EXISTS response_content;
