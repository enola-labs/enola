package rubyextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A job a cron schedule names, and a workflow a Temporal schedule names, are
// referenced by the config; other keys and lowercase values are not classes.
func TestScheduleConfigNamesTheClassesItRuns(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Gemfile":               "gem 'rails'\n",
		"config/application.rb": "module App\n  class Application < Rails::Application\n  end\nend\n",
		"config/schedule.yml": `purge_old:
  cron: "* * * * *"
  class: Parcels::PurgeOldJob
  description: Purges old parcels
sync_templates:
  cron: '0 * * * *'
  class: "::TemplateSync"
  queue: low
`,
		"config/temporal_schedule.yml":                "cleanup:\n  cron: \"0 8 * * 5\"\n  workflow: CleanupWorkflow\n  task_queue: general\n",
		"engines/billing/config/sidekiq_schedule.yml": "invoices:\n  class: Billing::InvoiceJob\n",
		"app/jobs/parcels/purge_old_job.rb":           "module Parcels\n  class PurgeOldJob < ApplicationJob\n    def perform; end\n  end\nend\n",
	})
	got := map[string][]string{}
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.PropBool("scheduler_config") {
			for _, r := range f.Relations {
				got[f.File] = append(got[f.File], r.Target)
			}
		}
	}
	for file, want := range map[string][]string{
		"config/schedule.yml":                         {"Parcels::PurgeOldJob", "TemplateSync"},
		"config/temporal_schedule.yml":                {"CleanupWorkflow"},
		"engines/billing/config/sidekiq_schedule.yml": {"Billing::InvoiceJob"},
	} {
		if !slices.Equal(got[file], want) {
			t.Errorf("%s names %v, want %v", file, got[file], want)
		}
	}
}
