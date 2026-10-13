package gateway

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Lengths an application's or model's name and description may run to. A
// description is a sentence or two that says what the application or model
// is for, shown under its name wherever it is chosen.
const (
	maxDetailsName        = 120
	maxDetailsDescription = 300
)

// detailsPatch is the body that renames an application or a model or
// changes its description (PATCH /api/admin/applications/{id},
// /api/admin/models/{id}): a field left out stays as it is.
type detailsPatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// clean trims what was sent and checks it: something must change, a name
// cannot be empty, and neither can run past its length.
func (p *detailsPatch) clean() error {
	if p.Name == nil && p.Description == nil {
		return errors.New("nothing to change: send a name or a description")
	}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return errors.New("the name cannot be empty")
		}
		if utf8.RuneCountInString(name) > maxDetailsName {
			return fmt.Errorf("the name can be at most %d characters", maxDetailsName)
		}
		p.Name = &name
	}
	if p.Description != nil {
		desc := strings.TrimSpace(*p.Description)
		if utf8.RuneCountInString(desc) > maxDetailsDescription {
			return fmt.Errorf("the description can be at most %d characters", maxDetailsDescription)
		}
		p.Description = &desc
	}
	return nil
}

// auditMetadata is what the audit row records: the fields that changed.
func (p detailsPatch) auditMetadata() map[string]string {
	md := map[string]string{}
	if p.Name != nil {
		md["name"] = *p.Name
	}
	if p.Description != nil {
		md["description"] = *p.Description
	}
	return md
}
