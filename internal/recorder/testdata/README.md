# Recording fixtures

## relabelled-session.cast

A real tamper artefact, recovered from the dev object store on 2026-09-11 and
kept here because it is worth more as a test than as a mystery.

It sat in the dev MinIO bucket under **two** names — the dashed and undashed
forms of session `3ae54ffe-7cd2-faf2-c920-559e98b9f713` — verifying against
neither. Nothing in git recorded what it was, so every full run of
`scripts/drill.sh` reported it as corruption and no one could say whether that
was a planted demonstration or a real incident.

It is a demonstration. The file's header claims

    "env": { "ARGUS_SESSION": "3ae54ffe7cd2faf2c920559e98b9f713", ... }

while its own trailer, written by the gateway at close, says

    argus: session f37acc687059e4f2750c542d1a1f44d5 recorded (chain 4fbe4e8d9778…)

Header and trailer name different sessions. No gateway produces that. This is
session `f37acc68`'s recording with its header rewritten by hand to impersonate
session `3ae54ffe` — the exact attack the hash chain exists to catch, which is
why it verifies against neither session's sealed head.

The session it claims to be lasted 89 ms, from `127.0.0.1`, and the gateway
reported 451 bytes for a file that is 453. Two bytes, which is about what
editing that header costs.

Keep it. `verify_test.go` asserts the chain still refuses it, in both
directions, so the day that stops being true something has gone wrong in
`Verify`.
