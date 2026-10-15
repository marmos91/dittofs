# Known failures

## Removing a row

A row may only be removed on the literal verdict line from the artifact:

```
WRT2 st_write.testSizes : PASS
| WRT2 | bug | example row inside a fence | #1234 |
```

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| WRT18     | bug      | change attr frozen across a write | #2382 |
| EID6g     | bug      | suffixed code, not just digits    | #2340 |
| CSESS16a  | bug      | another suffixed code             | #2340 |
| flock/*   | proto    | NLM not implemented               | -     |
