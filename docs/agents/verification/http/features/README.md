# Service feature map

| Feature | User outcome | Driver |
| --- | --- | --- |
| [Cache reuse and immutability](cache.md) | Store once, restore identical bytes, reject replacement | Direct driver |
| [Credential boundaries](access.md) | Trusted writers and read-only consumers retain distinct permissions | Direct driver |
| [Health and outages](outages.md) | Diagnose storage readiness and choose fail-open or fail-closed behavior | Direct driver, mode flag |
| [Stalled transfers](stalls.md) | A transfer that stops moving ends after the stall timeout instead of hanging Nx | Direct driver, storage relay |
| [Secure startup and shutdown](startup.md) | Reject missing credentials, serve trusted HTTPS, drain on termination | Direct driver, TLS flag |
| [Prometheus metrics](metrics.md) | Scrape request counts by outcome from a separate, optional port | Direct driver |
| [Nx consumer builds](nx-consumer.md) | Real builds upload, restore remotely, and rebuild through storage outages | Existing Nx harness |
| [Repository tiers](tiers.md) | PRs reuse their own results without poisoning trusted ones; repositories stay apart | Existing Nx harness, CI job |

Conditional-write concurrency and streaming boundary cases have additional
existing backend harnesses listed in the cache feature. Image, Helm, external
credential-chain, and production-provider behavior require their actual target
environment; local source verification does not claim those deployment proofs.
