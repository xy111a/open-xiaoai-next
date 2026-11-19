#!/usr/bin/env python3
"""打断（barge-in）状态机的 Python 模型——仅供本地验证逻辑用。

与 hub/cmd/hub/main.go 里的 DeviceState 状态机一一对应
（markSpeaking / clearSpeaking / handleWake / openSession /
sweepSessions / onArbitrationResult），不是生产代码，不被 Go 编译。
跑：python3 hub/cmd/hub/barge_in_state_model.py

Go 单测（barge_in_test.go）与模拟集成测试
（barge_in_integration_test.go）是 CI 里跑的正式版；
这里只是同一转移表在无 Go 工具链的本机上能跑的等价物。
"""

IDLE, LISTENING, SPEAKING = "idle", "listening", "speaking"


class Hub:
    """hubHandler 状态部分的最小模型：states + playGen + sessions。"""

    def __init__(self):
        self.states = {}
        self.play_gen = {}
        self.sessions = {}  # device -> deadline (float seconds, monotonic)

    def state(self, dev):
        return self.states.get(dev, IDLE)

    def has_live_session(self, dev, now):
        return dev in self.sessions and now < self.sessions[dev]

    def mark_speaking(self, dev):
        self.play_gen[dev] = self.play_gen.get(dev, 0) + 1
        self.states[dev] = SPEAKING
        return self.play_gen[dev]

    def clear_speaking(self, dev, gen, now):
        if self.play_gen.get(dev) != gen:
            return  # 旧代定时器：失效
        if self.states.get(dev) != SPEAKING:
            return
        self.states[dev] = LISTENING if self.has_live_session(dev, now) else IDLE

    def handle_wake(self, dev):
        """返回 True 表示调用方必须发停播命令（interrupt）。"""
        if self.states.get(dev) == SPEAKING:
            self.play_gen[dev] = self.play_gen.get(dev, 0) + 1
            self.states[dev] = LISTENING
            return True
        self.states[dev] = LISTENING
        return False

    def open_session(self, dev, now):
        self.sessions[dev] = now + 20.0
        self.states[dev] = LISTENING

    def sweep_sessions(self, now):
        for dev in [d for d, until in self.sessions.items() if now > until]:
            del self.sessions[dev]
            if self.states.get(dev) == LISTENING:
                self.states[dev] = IDLE

    def disconnect(self, dev):
        self.sessions.pop(dev, None)
        self.states.pop(dev, None)

    def arbitration_result(self, winner, losers):
        """简化版 onArbitrationResult：winner 在线；输家无会话才回 Idle。"""
        if not winner:
            return
        self.open_session(winner, 0.0)  # now 用 0 简化：只看转移，不看过期
        for dev in [winner] + losers:
            if dev == winner:
                continue
            if self.states.get(dev) == LISTENING and dev not in self.sessions:
                self.states[dev] = IDLE


def check(name, got, want):
    status = "PASS" if got == want else "FAIL"
    print(f"[{status}] {name}: got={got!r} want={want!r}")
    return got == want


def main():
    ok = True
    h = Hub()
    # 1. Speaking + 唤醒 -> Listening + 必须停播；旧代定时器失效。
    gen = h.mark_speaking("A")
    ok &= check("speaking", h.state("A"), SPEAKING)
    ok &= check("barge-in needs stop", h.handle_wake("A"), True)
    ok &= check("after barge-in", h.state("A"), LISTENING)
    h.clear_speaking("A", gen, 0.0)
    ok &= check("stale timer keeps listening", h.state("A"), LISTENING)
    ok &= check("second wake needs no stop", h.handle_wake("A"), False)

    # 2. Idle + 唤醒 -> Listening，不停播。
    ok &= check("unknown device idle", h.state("B"), IDLE)
    ok &= check("idle wake needs no stop", h.handle_wake("B"), False)
    ok &= check("idle wake -> listening", h.state("B"), LISTENING)

    # 3. 跨设备隔离：A 的唤醒不碰 B。
    h2 = Hub()
    h2.mark_speaking("B")
    ok &= check("A wake needs no stop", h2.handle_wake("A"), False)
    ok &= check("B still speaking", h2.state("B"), SPEAKING)
    ok &= check("B wake needs stop", h2.handle_wake("B"), True)

    # 4. 播完：会话活着回 Listening；会话过期（清扫）回 Idle。
    h3 = Hub()
    h3.open_session("A", 0.0)
    g = h3.mark_speaking("A")
    h3.clear_speaking("A", g, 5.0)
    ok &= check("finish in session -> listening", h3.state("A"), LISTENING)
    h3.sweep_sessions(25.0)  # 20s 会话过期，清扫把 Listening 翻回 Idle
    ok &= check("sweep expires to idle", h3.state("A"), IDLE)
    g = h3.mark_speaking("A")
    h3.clear_speaking("A", g, 26.0)
    ok &= check("finish without session -> idle", h3.state("A"), IDLE)

    # 5. 旧代定时器不得清新播报。
    h4 = Hub()
    g1 = h4.mark_speaking("A")
    g2 = h4.mark_speaking("A")
    h4.clear_speaking("A", g1, 0.0)
    ok &= check("stale gen keeps speaking", h4.state("A"), SPEAKING)
    h4.clear_speaking("A", g2, 0.0)
    ok &= check("current gen clears to idle", h4.state("A"), IDLE)

    # 6. 仲裁：赢家 Listening，输家（无会话）回 Idle。
    h5 = Hub()
    h5.mark_speaking("A")
    h5.handle_wake("A")
    h5.handle_wake("B")
    h5.arbitration_result("B", ["A"])
    ok &= check("loser without session -> idle", h5.state("A"), IDLE)
    ok &= check("winner listening", h5.state("B"), LISTENING)

    # 7. 断线清态。
    h6 = Hub()
    h6.mark_speaking("A")
    h6.open_session("A", 0.0)
    h6.disconnect("A")
    ok &= check("disconnect -> idle", h6.state("A"), IDLE)

    print("ALL PASS" if ok else "SOME FAILED")
    raise SystemExit(0 if ok else 1)


if __name__ == "__main__":
    main()
