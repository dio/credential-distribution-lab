# Security scope

This repository is a research harness. Do not deploy its unauthenticated baseline
fixture or treat its model as a production credential service. See SPEC.md,
SECURITY-REVIEW.md and RESEARCH.md for assumptions, limits and demonstrated attacks.

Tests use synthetic credentials and ephemeral in-memory identities. Never provide
production secrets to the test harness or include them in issue reports.

Report reproducible research defects in GitHub issues using synthetic inputs.
For a sensitive disclosure, use GitHub's private vulnerability reporting when
enabled for this repository. Fixes here do not imply remediation in any upstream
product; coordinate upstream reports with the affected project's maintainers.
