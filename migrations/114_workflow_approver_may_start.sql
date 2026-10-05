-- A workflow whose approver may also start it. Business admins decide
-- approval requests and do not submit them; a planning round is different:
-- the HR admin who owns eight of the HR planning round's nine steps and
-- signs it off is its natural starter (found 2026-10-05 rebuilding an HR
-- planning workbook, and 2026-10-04 with a sales target round). Off by
-- default, so request/approval workflows keep the separation of duties.
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS approver_may_start BOOLEAN NOT NULL DEFAULT false;
