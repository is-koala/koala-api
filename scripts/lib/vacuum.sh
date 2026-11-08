# Shared constants for the vacuum steps of api-lint. Sourced, never run.
#
# VACUUM_UNFINISHED is the single source of the signature vacuum prints
# when it does not run a rule to completion. Both scripts/api-lint.sh and
# scripts/api-lint-probe.sh refuse a run that carries it, and they must
# look for the same words: vacuum changing its wording while only one
# script is updated is exactly the "rule in N places" rot this repository
# keeps finding (ADR-0029 Karar 12). One definition, two readers.
VACUUM_UNFINISHED='Rule timed out, skipping|timed out after'
