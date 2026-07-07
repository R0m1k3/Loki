"""Routes du benchmark de modèles."""
from __future__ import annotations

import json

from fastapi import APIRouter
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from .. import bench

router = APIRouter(prefix="/api/bench", tags=["bench"])


class BenchRequest(BaseModel):
    model: str


@router.get("")
async def scores() -> dict:
    return {"scores": bench.get_scores()}


@router.post("")
async def run(req: BenchRequest) -> StreamingResponse:
    async def event_stream():
        try:
            async for ev in bench.run_bench(req.model):
                etype = ev.pop("type")
                yield f"event: {etype}\ndata: {json.dumps(ev, ensure_ascii=False)}\n\n"
        except Exception as exc:
            payload = json.dumps(
                {"message": f"benchmark interrompu : {str(exc)[:200]}"},
                ensure_ascii=False,
            )
            yield f"event: error\ndata: {payload}\n\n"

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache, no-transform", "X-Accel-Buffering": "no"},
    )
