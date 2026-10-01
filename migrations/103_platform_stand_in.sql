-- A platform admin or a platform-wide builder acting on a dedicated tenant —
-- its id in X-Tenant-Id, or one of its applications in X-App-Id — is
-- resolved from the control plane, where their account and roles are. What
-- they write in the tenant's database names them through foreign keys to
-- identity.user, so that database holds a stand-in row under the same id,
-- with a subject and address of its own: no tenant, no role, no grant, not
-- listed and not counted (internal/gateway/stand_in.go). Nothing in the
-- tenant's database decides what they may do; a stand-in that held a role
-- there would be trusted by whatever reads roles from it.
ALTER TABLE identity."user" ADD COLUMN stand_in BOOLEAN NOT NULL DEFAULT FALSE;

-- A stand-in carries a reserved, never-delivered address built from the
-- platform admin's id, and nothing else may: an account a tenant made under
-- it first would keep the stand-in from being written.
ALTER TABLE identity."user" ADD CONSTRAINT stand_in_address_reserved
    CHECK (stand_in = (lower(email) LIKE '%@stand-in.invalid'));

CREATE FUNCTION identity.refuse_stand_in_holding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM identity."user" WHERE id = NEW.user_id AND stand_in) THEN
        RAISE EXCEPTION 'a platform admin''s stand-in holds no role, grant or membership'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER stand_in_holds_no_role BEFORE INSERT OR UPDATE OF user_id ON identity.role_assignment
    FOR EACH ROW EXECUTE FUNCTION identity.refuse_stand_in_holding();
CREATE TRIGGER stand_in_holds_no_app BEFORE INSERT OR UPDATE OF user_id ON identity.user_app_access
    FOR EACH ROW EXECUTE FUNCTION identity.refuse_stand_in_holding();
CREATE TRIGGER stand_in_holds_no_model BEFORE INSERT OR UPDATE OF user_id ON identity.user_model_access
    FOR EACH ROW EXECUTE FUNCTION identity.refuse_stand_in_holding();
CREATE TRIGGER stand_in_holds_no_business_role BEFORE INSERT OR UPDATE OF user_id ON identity.business_role_member
    FOR EACH ROW EXECUTE FUNCTION identity.refuse_stand_in_holding();
