package config

import (
	"slices"
	"testing"
)

func TestWriteKeySecrets(t *testing.T) {
	workflows := map[string][]byte{
		".github/workflows/distribute.yml": []byte(`on: {schedule: [{cron: "0 5 * * *"}]}
jobs:
  probe:
    steps:
      - run: echo "v=$V" >> "$GITHUB_OUTPUT"
        env: { V: "${{ secrets.GH_WRITER_KEY != '' }}" }
  distribute:
    environment: touchmark-distribute
    env:
      TOUCHMARK_GH_WRITE_APP_ID: ${{ vars.APP_ID }}
      TOUCHMARK_GH_WRITE_APP_KEY: ${{ secrets.GH_WRITER_KEY }}
    steps:
      - uses: acme/touchmark-action@v1
        with:
          corp-write-token: ${{ secrets.CORP_TOKEN }}
          signing-key: ${{ secrets.SIGNING }}
          github-token: ${{ secrets.GITHUB_TOKEN }}
`),
		".github/workflows/broken.yml": []byte("jobs: [\n"),
	}
	got := WriteKeySecrets(workflows)
	if want := []string{"CORP_TOKEN", "GH_WRITER_KEY", "SIGNING"}; !slices.Equal(got, want) {
		t.Errorf("WriteKeySecrets = %q, want %q", got, want)
	}
	if got := WriteKeySecrets(nil); len(got) != 0 {
		t.Errorf("no workflows: %q", got)
	}
}
