-- Add awaiting_user state to work_item_state enum.
-- This state is used when a work item is waiting for user input before continuing.
-- The work item's previous_state is stored to restore it when the user answers.

ALTER TYPE work_item_state ADD VALUE IF NOT EXISTS 'awaiting_user' BEFORE 'blocked';
