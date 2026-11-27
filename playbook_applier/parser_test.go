package playbook_applier

import (
	"strings"
	"testing"
)

// Captured from a real ansible-core 2.22 run (colors stripped).
const demoOutput = `PLAY [Demo] ********************************************************************

TASK [Write file] **************************************************************
[WARNING]: Host 'h2' is using the discovered Python interpreter at '/usr/bin/python3.15', but future installation of another Python interpreter could cause a different interpreter to be discovered. See https://docs.ansible.com/ansible-core/2.22/reference_appendices/interpreter_discovery.html for more information.
[WARNING]: Host 'h1' is using the discovered Python interpreter at '/usr/bin/python3.15', but future installation of another Python interpreter could cause a different interpreter to be discovered. See https://docs.ansible.com/ansible-core/2.22/reference_appendices/interpreter_discovery.html for more information.
--- before
+++ after: /tmp/opencode/fmt/out.txt
@@ -0,0 +1 @@
+hello v2

changed: [h2]
ok: [h1]

TASK [Fail task] ***************************************************************
[ERROR]: Task failed: Action failed: boom on purpose
Origin: /tmp/opencode/fmt/demo.yml:10:7

 8         content: "hello v2\n"
 9         dest: /tmp/opencode/fmt/out.txt
10     - name: Fail task
         ^ column 7

fatal: [h1]: FAILED! => {"changed": false, "msg": "boom on purpose"}
fatal: [h2]: FAILED! => {"changed": false, "msg": "boom on purpose"}

PLAY RECAP *********************************************************************
h1                         : ok=1    changed=0    unreachable=0    failed=1    skipped=0    rescued=0    ignored=0   
h2                         : ok=1    changed=1    unreachable=0    failed=1    skipped=0    rescued=0    ignored=0`

func TestParseDemo(t *testing.T) {
	summaries, tasks := Parse(demoOutput)

	if len(summaries) != 2 {
		t.Fatalf("expected 2 recap lines, got %d", len(summaries))
	}
	want := map[string]HostSummary{
		"h1": {Host: "h1", Ok: 1, Failed: 1},
		"h2": {Host: "h2", Ok: 1, Changed: 1, Failed: 1},
	}
	for _, s := range summaries {
		w, ok := want[s.Host]
		if !ok {
			t.Fatalf("unexpected host %q in recap", s.Host)
		}
		if s != w {
			t.Errorf("recap for %q: got %+v want %+v", s.Host, s, w)
		}
	}

	if len(tasks) != 4 {
		t.Fatalf("expected 4 task results, got %d: %+v", len(tasks), tasks)
	}
	byKey := map[string]TaskResult{}
	for _, tr := range tasks {
		byKey[tr.Host+"/"+tr.Task] = tr
	}

	wf := byKey["h2/Write file"]
	if wf.Status != "changed" {
		t.Errorf("h2/Write file: status = %q, want changed", wf.Status)
	}
	if !strings.Contains(wf.Diff, "+hello v2") {
		t.Errorf("h2/Write file: diff = %q, want it to contain +hello v2", wf.Diff)
	}

	if ok := byKey["h1/Write file"]; ok.Status != "ok" {
		t.Errorf("h1/Write file: status = %q, want ok", ok.Status)
	}

	f1 := byKey["h1/Fail task"]
	if f1.Status != "failed" || f1.Error != "boom on purpose" {
		t.Errorf("h1/Fail task: got %+v, want failed with msg", f1)
	}
}

// Captured from a second real run: two hosts both changed, one diff block per
// host, plus skipping and multi-line JSON.
const demo2Output = `PLAY [Demo2] *******************************************************************

TASK [Skip task] ***************************************************************
skipping: [h1]
skipping: [h2]

TASK [Write file again] ********************************************************
--- before
+++ after: /tmp/opencode/fmt/out2.txt
@@ -0,0 +1 @@
+hello v3

changed: [h1]
--- before
+++ after: /tmp/opencode/fmt/out2.txt
@@ -0,0 +1 @@
+hello v3

changed: [h2]

TASK [Last task] ***************************************************************
ok: [h1] => {
    "msg": "done"
}
ok: [h2] => {
    "msg": "done"
}

PLAY RECAP *********************************************************************
h1                         : ok=2    changed=1    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0   
h2                         : ok=2    changed=1    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0`

func TestParseDemo2(t *testing.T) {
	summaries, tasks := Parse(demo2Output)

	if len(summaries) != 2 {
		t.Fatalf("expected 2 recap lines, got %d", len(summaries))
	}
	if s := summaries[0]; s.Ok != 2 || s.Changed != 1 || s.Skipped != 1 {
		t.Errorf("h1 recap: got %+v, want ok=2 changed=1 skipped=1", s)
	}

	byKey := map[string]TaskResult{}
	for _, tr := range tasks {
		byKey[tr.Host+"/"+tr.Task] = tr
	}

	for _, host := range []string{"h1", "h2"} {
		if sk := byKey[host+"/Skip task"]; sk.Status != "skipped" {
			t.Errorf("%s/Skip task: status = %q, want skipped", host, sk.Status)
		}
		wf := byKey[host+"/Write file again"]
		if wf.Status != "changed" || !strings.Contains(wf.Diff, "+hello v3") {
			t.Errorf("%s/Write file again: got %+v, want changed with diff", host, wf)
		}
		if last := byKey[host+"/Last task"]; last.Status != "ok" {
			t.Errorf("%s/Last task: status = %q, want ok", host, last.Status)
		}
	}
	if len(tasks) != 6 {
		t.Errorf("expected 6 task results, got %d: %+v", len(tasks), tasks)
	}
}

func TestParseEmpty(t *testing.T) {
	summaries, tasks := Parse("")
	if len(summaries) != 0 || len(tasks) != 0 {
		t.Fatalf("expected empty results, got %+v / %+v", summaries, tasks)
	}
}

func TestParseUnreachable(t *testing.T) {
	out := `PLAY [X] ****
TASK [t] ***
unreachable: [h1]
PLAY RECAP ****
h1 : ok=0    changed=0    unreachable=1    failed=0    skipped=0    rescued=0    ignored=0`
	summaries, tasks := Parse(out)
	if len(summaries) != 1 || summaries[0].Unreachable != 1 {
		t.Fatalf("recap: %+v", summaries)
	}
	if len(tasks) != 1 || tasks[0].Status != "unreachable" || tasks[0].Host != "h1" {
		t.Fatalf("tasks: %+v", tasks)
	}
}

func TestParseANSI(t *testing.T) {
	out := "\x1b[0mPLAY [X]\x1b[0m\n\x1b[32mok: [h1]\x1b[0m\n"
	_, tasks := Parse(out)
	if len(tasks) != 1 || tasks[0].Host != "h1" || tasks[0].Status != "ok" {
		t.Fatalf("tasks: %+v", tasks)
	}
}
