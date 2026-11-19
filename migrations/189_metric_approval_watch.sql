-- Whether an approval keeps a metric's figures (Integrated FP&A build,
-- 2026-11-19). An approved run keeps every figure in its member scope and
-- warns "changed since approval" when one moves. A metric that is a view
-- moving with the calendar — year to date read at the last closed month, the
-- sum over a rolling window — moved at every month-end, so an approved budget
-- reported 219 changed figures after an ordinary close and the warning became
-- noise. false: approvals do not keep the metric, and its changes are not
-- reported.
ALTER TABLE model.metric_def
    ADD COLUMN IF NOT EXISTS approval_watch boolean NOT NULL DEFAULT true;
