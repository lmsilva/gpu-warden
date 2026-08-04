package expose

import (
	"strings"
	"testing"

	"github.com/lmsilva/gpu-warden/internal/report"
	"github.com/lmsilva/gpu-warden/internal/slurmapi"
)

func TestWriteEscapesLabels(t *testing.T) {
	var sb strings.Builder
	Write(&sb, []report.JobReport{{Job: slurmapi.Job{JobID: 1,
		UserName: "evil\"} bad{", Partition: "p"}, HasData: true}})
	out := sb.String()
	if strings.Contains(out, `user="evil"`) || !strings.Contains(out, `evil\"`) {
		t.Errorf("label value not escaped:\n%s", out)
	}
}
