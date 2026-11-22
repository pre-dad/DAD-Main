-- Generated dashboard layouts (UI catalog specs) and their approval. A spec is
-- drafted by one staff member and goes live only when a different one approves
-- it; at most one spec is live at a time.
CREATE TABLE donor_insight_specs (
	id UUID NOT NULL,
	spec JSONB NOT NULL,
	catalog_version INTEGER NOT NULL,
	state VARCHAR(16) NOT NULL,
	author_id UUID NOT NULL,
	approver_id UUID,
	retired_by UUID,
	reject_reason VARCHAR(500),
	created_at TIMESTAMP WITH TIME ZONE NOT NULL,
	updated_at TIMESTAMP WITH TIME ZONE NOT NULL,
	submitted_at TIMESTAMP WITH TIME ZONE,
	decided_at TIMESTAMP WITH TIME ZONE,
	retired_at TIMESTAMP WITH TIME ZONE,
	PRIMARY KEY (id),
	FOREIGN KEY (author_id) REFERENCES donor_staff (id),
	FOREIGN KEY (approver_id) REFERENCES donor_staff (id),
	FOREIGN KEY (retired_by) REFERENCES donor_staff (id),
	CONSTRAINT ck_donor_insight_specs_state CHECK (state IN ('draft', 'pending', 'live', 'retired', 'rejected')),
	CONSTRAINT ck_donor_insight_specs_catalog_version CHECK (catalog_version >= 1),
	CONSTRAINT ck_donor_insight_specs_array CHECK (jsonb_typeof(spec) = 'array'),
	CONSTRAINT ck_donor_insight_specs_two_person CHECK (approver_id IS NULL OR approver_id <> author_id),
	CONSTRAINT ck_donor_insight_specs_decision CHECK (
		(state IN ('draft', 'pending')) = (approver_id IS NULL AND decided_at IS NULL)
	),
	CONSTRAINT ck_donor_insight_specs_reason CHECK ((state = 'rejected') = (reject_reason IS NOT NULL)),
	CONSTRAINT ck_donor_insight_specs_retired CHECK ((state = 'retired') = (retired_by IS NOT NULL AND retired_at IS NOT NULL))
);

CREATE UNIQUE INDEX ux_donor_insight_specs_live ON donor_insight_specs ((true)) WHERE state = 'live';
CREATE INDEX ix_donor_insight_specs_created ON donor_insight_specs (created_at DESC, id);
