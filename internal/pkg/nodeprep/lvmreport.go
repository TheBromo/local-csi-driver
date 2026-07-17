// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"encoding/json"
	"fmt"
)

// lvmReportOutput is the top-level structure of `pvs`/`vgs`
// --reportformat json output.
type lvmReportOutput struct {
	Report []map[string][]map[string]string `json:"report"`
}

// lvmReport runs an LVM reporting command (pvs or vgs) on the host with a
// select filter and returns the rows of the given report section ("pv" or
// "vg").
func (p *Preparer) lvmReport(ctx context.Context, command, section, fields, selector string) ([]map[string]string, error) {
	out, err := p.host.Run(ctx, command,
		"--reportformat", "json",
		"--units", "b",
		"--nosuffix",
		"--options", fields,
		"--select", selector)
	if err != nil {
		return nil, fmt.Errorf("failed to run %s: %w", command, err)
	}
	var report lvmReportOutput
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("failed to parse %s output: %w", command, err)
	}
	var rows []map[string]string
	for _, r := range report.Report {
		rows = append(rows, r[section]...)
	}
	return rows, nil
}
