"""
Run code in a Vora sandbox with the Python SDK.

    export VORA_API_URL=http://127.0.0.1:9300
    export VORA_API_KEY=vora_...
    pip install -e sdk/python
    python sdk/python/examples/basic.py
"""

import asyncio

import vora


async def main() -> None:
    async with vora.Vora() as client:
        sandbox = await client.sandboxes.create(image="vora/python", memory="512Mi", network="none")
        try:
            await sandbox.write_file("/workspace/main.py", "print(6 * 7)\n")
            result = await sandbox.exec("python main.py", cwd="/workspace")
            print(f"exit {result.exit_code}: {result.stdout.strip()}")

            print("streamed:")
            async for chunk in sandbox.stream_exec("echo one; echo two"):
                if chunk.kind == "stdout":
                    print(f"  {chunk.data.decode().strip()}")

            snapshot = await client.snapshots.create(sandbox.id)
            print(f"snapshot {snapshot.id} captured {snapshot.info.bytes} bytes")
        finally:
            await sandbox.destroy()


if __name__ == "__main__":
    asyncio.run(main())
