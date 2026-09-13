# Raw results

Every run writes two files here:

- `<timestamp>-<label>.json` — the load generator's record: configuration,
  latency distribution, enforcement error, validity verdict and warnings.
- `<timestamp>-<label>-server.json` — the service's own view at the same
  moment: admitted/denied counts and server-side handler latency.

These are committed deliberately. A results table in a README is a claim; the
raw JSON behind it is the evidence, and it lets a run be re-examined later
without paying to reproduce it.

**Check `"valid": false` before using a run.** The generator marks a run invalid
when it shed requests, failed to achieve its offered rate, or saw more than 1%
failures. The `warnings` field says which.

`scratch/` is gitignored, for throwaway runs.
