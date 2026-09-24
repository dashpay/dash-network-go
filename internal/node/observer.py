"""Read-only compatibility adapter, loaded after the retained mutation worker.

An RPC response-format correction must not require changing an existing network's
genesis/configuration recipe. The controller selects this adapter only for reads;
the worker also rejects mutation actions before instance or filesystem access.
Keep the response decoder aligned with Worker.tenderdash (tested together).
"""


class ReadOnlyWorker(Worker):
    def execute(self):
        self.require(
            self.q["action"] in ["inspect", "core-status", "platform-status"],
            "observation-only",
        )
        return super().execute()

    def tenderdash(self, method):
        with self.opener.open(
            "http://127.0.0.1:" + str(self.ports["platformRPC"]) + "/" + method,
            timeout=15,
        ) as response:
            raw = response.read(1024 * 1024 + 1)
        self.require(len(raw) <= 1024 * 1024, "tenderdash-response-size")
        value = json.loads(raw)
        self.require(
            isinstance(value, dict) and value.get("error") is None,
            "tenderdash-rpc-failed",
        )
        result = value.get("result", value)
        fields = {"status": ["node_info", "sync_info", "validator_info"],
                  "block": ["block_id", "block"]}.get(method.split("?", 1)[0])
        self.require(
            fields is not None and isinstance(result, dict)
            and all(isinstance(result.get(key), dict) for key in fields),
            "tenderdash-response-shape",
        )
        return result


# The retained worker's main() resolves this class in its own execution scope.
# That scope has __name__ != '__main__' until this guarded adapter is installed.
Worker = ReadOnlyWorker
