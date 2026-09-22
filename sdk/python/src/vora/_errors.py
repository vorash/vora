"""A failed Vora API call, or a malformed request built by this SDK."""

from __future__ import annotations

from typing import Any


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
