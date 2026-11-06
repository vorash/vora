"""A failed Vora API call, or a malformed request built by this SDK."""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from ._types import PoolRun


class VoraError(Exception):
    """A failed Vora API call, or a malformed request built by this SDK."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        body: Any = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        """HTTP status, when the failure came from the API."""
        self.body = body
        """Parsed response body (``{"error": "..."}``), when the API returned one."""


class RetryLaterError(VoraError):
    """The API refused for now and may name a wait (``Retry-After``)."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        body: Any = None,
        retry_after: float | None = None,
    ) -> None:
        super().__init__(message, status=status, body=body)
        self.retry_after = retry_after
        """Seconds to wait, when the API said."""


class RateLimitedError(RetryLaterError):
    """429: a pool with no free sandbox, or capacity reached. Nothing ran."""


class UnavailableError(RetryLaterError):
    """503: no sandbox could start, or a runner is down or still starting. Nothing ran."""


class GatewayTimeoutError(RetryLaterError):
    """504: the runner did not answer in time. It is not known whether anything ran."""


class ConflictError(VoraError):
    """409: the resource's state refuses this call."""


class PoolDrainingError(ConflictError):
    """The pool is being deleted. Retry later, or declare it again once it is gone.

    ``delete()`` also raises this, with status 202, when its wait runs out.
    """


class PoolNotLoadedError(ConflictError):
    """The runner refused to load this pool at its last boot. Retrying won't help."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        body: Any = None,
        reason: str | None = None,
    ) -> None:
        super().__init__(message, status=status, body=body)
        self.reason = reason
        """``not_loaded_reason`` from the pool's record, for example ``backend_missing``."""


class PoolSpecConflictError(ConflictError):
    """A live pool of this name has a different spec. Specs are immutable."""


class RequestTooLargeError(VoraError):
    """A run's encoded body is over the API's 1 MiB limit. It was not sent."""


class PoolRunError(VoraError):
    """A pool run that started and ended without an ``exit`` frame.

    The command may have run, so retrying is the caller's decision. ``code`` is
    ``client_stalled``, ``runner_unavailable`` or ``run_failed`` from the API's last
    line, or ``stream_ended`` when the connection ended with no such line.
    """

    def __init__(self, code: str, message: str, *, run: "PoolRun | None" = None) -> None:
        super().__init__(message)
        self.code = code
        self.run = run
        """The run's ``PoolRun`` frame, when one arrived."""
        self.stdout = ""
        """Output received before the failure. ``Pool.run`` fills it in."""
        self.stderr = ""
        """Output received before the failure. ``Pool.run`` fills it in."""
