// Cloud Native Pong client: online WebSocket play, local heuristic AI, and touch controls.
(function () {
    'use strict';

    const params = new URLSearchParams(window.location.search);
    const roomId = params.get('room');
    const playerName = params.get('name') || 'Player';
    const mode = params.get('mode') || 'local';
    const isAI = mode === 'ai';

    const roomLabel = document.getElementById('roomLabel');
    const statusElement = document.getElementById('status');
    const controlHint = document.getElementById('controlHint');
    const score1Element = document.getElementById('score1');
    const score2Element = document.getElementById('score2');
    const winnerOverlay = document.getElementById('winnerOverlay');
    const winnerText = document.getElementById('winnerText');
    const canvas = document.getElementById('pongCanvas');

    if (!canvas || !roomLabel || !statusElement) return;

    if (!isAI && !roomId) {
        document.body.innerHTML = '<h1 style="text-align:center;margin-top:80px">Missing room ID</h1>';
        return;
    }

    document.body.dataset.gameMode = isAI ? 'ai' : 'online';
    roomLabel.textContent = isAI ? 'vs Computer · Local AI' : 'Room: ' + roomId;

    let ws = null;
    let connection = null;
    let player = isAI ? 1 : 0;
    let gameState = null;
    let gameOverShown = false;
    let lastRenderedState = null;

    // Network delivery is not perfectly periodic through the Kubernetes
    // WebSocket proxy. A short authoritative-state buffer hides packet bursts
    // without changing the server-authoritative game decisions.
    const stateBuffer = [];
    const interpolationDelay = 50;

    const keys = Object.create(null);
    const touchInput = { up: false, down: false };
    const movementKeys = new Set(['w', 'W', 's', 'S', 'ArrowUp', 'ArrowDown']);

    const ctx = canvas.getContext('2d');
    const W = canvas.width;
    const H = canvas.height;
    const PADDLE_WIDTH = 0.02;
    const PADDLE_HEIGHT = 0.15;
    const BALL_SIZE = 0.025;
    const PADDLE_SPEED = 0.025;
    const BASE_BALL_SPEED = 0.012;
    const MAX_BALL_SPEED = 0.025;
    const WIN_SCORE = 7;
    const TICK_MS = 16;

    function setControlHint() {
        if (!controlHint) return;
        const keyboard = player === 2 ? 'Arrow Up/Down' : 'W/S';
        controlHint.textContent = isAI
            ? `Playing vs Computer · ${keyboard} or touch buttons`
            : `Controls: ${keyboard} or touch buttons`;
    }

    function updateScore(state) {
        score1Element.textContent = state.score1;
        score2Element.textContent = state.score2;
    }

    function showWinner(winner) {
        if (gameOverShown || !winnerOverlay || !winnerText) return;
        gameOverShown = true;
        winnerText.textContent = winner === player ? '🎉 You Win!' : '💀 You Lose!';
        winnerOverlay.classList.remove('hidden');
        statusElement.textContent = 'Game Over';
        if (connection) connection.close();
    }

    function bindTouchButton(id, direction) {
        const button = document.getElementById(id);
        if (!button) return;

        const setPressed = (pressed) => {
            touchInput[direction] = pressed;
            button.setAttribute('aria-pressed', String(pressed));
        };
        const release = () => setPressed(false);

        button.addEventListener('pointerdown', (event) => {
            event.preventDefault();
            setPressed(true);
            if (button.setPointerCapture && event.isTrusted) {
                try {
                    button.setPointerCapture(event.pointerId);
                } catch {
                    // Synthetic test events and older browsers may not expose
                    // an active pointer to capture; the pressed state still
                    // remains valid until pointerup/pointercancel.
                }
            }
        });
        button.addEventListener('pointerup', release);
        button.addEventListener('pointercancel', release);
        button.addEventListener('lostpointercapture', release);
        button.addEventListener('contextmenu', (event) => event.preventDefault());
    }

    bindTouchButton('moveUp', 'up');
    bindTouchButton('moveDown', 'down');

    function clearInput() {
        for (const key of movementKeys) keys[key] = false;
        touchInput.up = false;
        touchInput.down = false;
    }

    document.addEventListener('keydown', (event) => {
        if (!movementKeys.has(event.key)) return;
        event.preventDefault();
        keys[event.key] = true;
    });
    document.addEventListener('keyup', (event) => {
        if (!movementKeys.has(event.key)) return;
        event.preventDefault();
        keys[event.key] = false;
    });
    window.addEventListener('blur', clearInput);
    document.addEventListener('visibilitychange', () => {
        if (document.hidden) clearInput();
    });

    function readLocalInput() {
        if (player === 2) {
            return {
                up: Boolean(keys.ArrowUp || touchInput.up),
                down: Boolean(keys.ArrowDown || touchInput.down),
            };
        }
        return {
            up: Boolean(keys.w || keys.W || touchInput.up),
            down: Boolean(keys.s || keys.S || touchInput.down),
        };
    }

    function handleMessage(message) {
        if (message.type === 'joined') {
            player = message.player;
            setControlHint();
            statusElement.textContent =
                'You are Player ' + player + '. ' +
                (player === 1 ? 'Use W/S or touch buttons.' : 'Use ↑/↓ or touch buttons.') +
                (player === 1 ? ' Waiting for opponent...' : '');
        }

        if (message.type === 'state' && message.state && message.state.ball) {
            const state = message.state;
            const receivedAt = performance.now();
            gameState = state;
            stateBuffer.push({ state, receivedAt });
            while (stateBuffer.length > 8 ||
                (stateBuffer.length > 2 && receivedAt - stateBuffer[0].receivedAt > 250)) {
                stateBuffer.shift();
            }

            if (state.status === 'playing') statusElement.textContent = 'Playing!';
            updateScore(state);
            if (state.status === 'finished') showWinner(state.winner);
        }

        if (message.type === 'error') {
            statusElement.textContent = 'Error: ' + (message.message || 'game unavailable');
        }
    }

    function markDisconnected(message) {
        if (!gameOverShown) statusElement.textContent = message;
        connection = null;
    }

    function connectWebSocket() {
        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const wsURL = `${protocol}//${window.location.host}/rooms/${encodeURIComponent(roomId)}/ws`;
        ws = new WebSocket(wsURL);
        connection = {
            send: (value) => ws.send(JSON.stringify(value)),
            close: () => ws.close(),
            isOpen: () => ws.readyState === WebSocket.OPEN,
        };

        ws.onopen = function () {
            statusElement.textContent = 'Connected via WebSocket. Waiting for opponent...';
            ws.send(JSON.stringify({ type: 'proxy-ready' }));
        };
        ws.onmessage = function (event) {
            try {
                handleMessage(JSON.parse(event.data));
            } catch {
                statusElement.textContent = 'Received invalid game data.';
            }
        };
        ws.onclose = () => markDisconnected('Disconnected.');
        ws.onerror = () => {
            if (!gameOverShown) statusElement.textContent = 'Connection error.';
        };
    }

    function frameWebTransportMessage(value) {
        const payload = new TextEncoder().encode(JSON.stringify(value));
        const frame = new Uint8Array(4 + payload.length);
        new DataView(frame.buffer).setUint32(0, payload.length);
        frame.set(payload, 4);
        return frame;
    }

    async function connectWebTransport(url) {
        const transport = new WebTransport(url);
        await transport.ready;
        const stream = await transport.createBidirectionalStream();
        const writer = stream.writable.getWriter();
        const reader = stream.readable.getReader();
        const decoder = new TextDecoder();
        let writeChain = Promise.resolve();
        let buffered = new Uint8Array(0);
        let open = true;

        const connectionForTransport = {
            send(value) {
                writeChain = writeChain.then(() => writer.write(frameWebTransportMessage(value)));
                writeChain.catch(() => markDisconnected('WebTransport write failed.'));
            },
            close() {
                open = false;
                transport.close();
                writer.releaseLock();
                reader.releaseLock();
            },
            isOpen: () => open,
        };
        connection = connectionForTransport;
        statusElement.textContent = 'Connected via WebTransport. Waiting for opponent...';
        connection.send({ type: 'proxy-ready' });

        (async () => {
            try {
                while (open) {
                    const result = await reader.read();
                    if (result.done) break;
                    const next = new Uint8Array(buffered.length + result.value.length);
                    next.set(buffered);
                    next.set(result.value, buffered.length);
                    buffered = next;
                    while (buffered.length >= 4) {
                        const length = new DataView(buffered.buffer, buffered.byteOffset).getUint32(0);
                        if (length > 1 << 20) throw new Error('message too large');
                        if (buffered.length < 4 + length) break;
                        const payload = buffered.slice(4, 4 + length);
                        buffered = buffered.slice(4 + length);
                        handleMessage(JSON.parse(decoder.decode(payload)));
                    }
                }
            } catch {
                if (open) markDisconnected('WebTransport connection error.');
            } finally {
                open = false;
                if (!gameOverShown) {
                    transport.closed.catch(() => undefined);
                    markDisconnected('Disconnected.');
                }
            }
        })();
    }

    async function connect() {
        if (window.WebTransport) {
            try {
                const response = await fetch('/api/capabilities', { headers: { accept: 'application/json' } });
                const capabilities = await response.json();
                if (capabilities.webtransport && capabilities.webtransport_url) {
                    const url = capabilities.webtransport_url.replace('{room}', encodeURIComponent(roomId));
                    await connectWebTransport(url);
                    return;
                }
            } catch {
                // Capability discovery or QUIC setup failed; use the tested
                // WebSocket fallback without delaying the game unnecessarily.
            }
        }
        connectWebSocket();
    }

    // Send input to the authoritative server at approximately 60Hz.
    setInterval(() => {
        if (isAI || !connection || !connection.isOpen() || !player) return;
        const input = readLocalInput();
        connection.send({ player, up: input.up, down: input.down });
    }, TICK_MS);

    function clampPaddle(value) {
        const halfHeight = PADDLE_HEIGHT / 2;
        return Math.max(halfHeight, Math.min(1 - halfHeight, value));
    }

    function movePaddle(paddle, input, speed) {
        if (input.up) paddle.y -= speed;
        if (input.down) paddle.y += speed;
        paddle.y = clampPaddle(paddle.y);
    }

    function reflectedY(value) {
        const wrapped = ((value % 2) + 2) % 2;
        return wrapped <= 1 ? wrapped : 2 - wrapped;
    }

    function resetBall(state, direction) {
        const variation = ((state.score1 + state.score2) % 5 - 2) * 0.004;
        const angle = variation;
        state.ball = {
            x: 0.5,
            y: 0.5,
            dx: direction * BASE_BALL_SPEED * Math.cos(angle),
            dy: BASE_BALL_SPEED * Math.sin(angle),
        };
    }

    function clampBallSpeed(state) {
        const speed = Math.hypot(state.ball.dx, state.ball.dy);
        if (speed === 0) {
            state.ball.dx = BASE_BALL_SPEED;
            return;
        }
        if (speed > MAX_BALL_SPEED) {
            const scale = MAX_BALL_SPEED / speed;
            state.ball.dx *= scale;
            state.ball.dy *= scale;
        } else if (speed < BASE_BALL_SPEED) {
            const scale = BASE_BALL_SPEED / speed;
            state.ball.dx *= scale;
            state.ball.dy *= scale;
        }
    }

    // A deliberately small, deterministic opponent. It predicts the ball's
    // next intersection with the right paddle, updates its target roughly six
    // times per second, and moves slightly slower than a human paddle. No LLM,
    // network request, model, or external service is involved.
    let aiTarget = 0.5;
    let aiTick = 0;

    function predictAIY(state) {
        if (state.ball.dx <= 0) return 0.5;
        const targetX = 1 - PADDLE_WIDTH - BALL_SIZE / 2;
        const ticksUntilPaddle = Math.max(0, (targetX - state.ball.x) / state.ball.dx);
        return reflectedY(state.ball.y + state.ball.dy * ticksUntilPaddle);
    }

    function createAIState() {
        const state = {
            ball: { x: 0.5, y: 0.5, dx: BASE_BALL_SPEED, dy: 0.004 },
            p1: { y: 0.5 },
            p2: { y: 0.5 },
            score1: 0,
            score2: 0,
            status: 'playing',
            winner: 0,
            p1_ready: true,
            p2_ready: true,
        };
        resetBall(state, 1);
        return state;
    }

    function tickAI() {
        const state = gameState;
        if (!state || state.status !== 'playing') return;

        movePaddle(state.p1, readLocalInput(), PADDLE_SPEED);

        aiTick += 1;
        if (aiTick % 6 === 0) aiTarget = predictAIY(state);
        const aiDifference = aiTarget - state.p2.y;
        if (Math.abs(aiDifference) > 0.006) {
            const aiSpeed = 0.019;
            state.p2.y = clampPaddle(state.p2.y + Math.sign(aiDifference) * Math.min(aiSpeed, Math.abs(aiDifference)));
        }

        const ball = state.ball;
        ball.x += ball.dx;
        ball.y += ball.dy;

        if (ball.y - BALL_SIZE / 2 <= 0) {
            ball.y = BALL_SIZE / 2;
            ball.dy = Math.abs(ball.dy);
        }
        if (ball.y + BALL_SIZE / 2 >= 1) {
            ball.y = 1 - BALL_SIZE / 2;
            ball.dy = -Math.abs(ball.dy);
        }

        if (ball.x - BALL_SIZE / 2 <= PADDLE_WIDTH && ball.dx < 0) {
            const offset = (ball.y - state.p1.y) / (PADDLE_HEIGHT / 2);
            if (Math.abs(offset) <= 1) {
                ball.x = PADDLE_WIDTH + BALL_SIZE / 2;
                ball.dx = Math.abs(ball.dx);
                ball.dy += offset * 0.005;
                clampBallSpeed(state);
            }
        }

        if (ball.x + BALL_SIZE / 2 >= 1 - PADDLE_WIDTH && ball.dx > 0) {
            const offset = (ball.y - state.p2.y) / (PADDLE_HEIGHT / 2);
            if (Math.abs(offset) <= 1) {
                ball.x = 1 - PADDLE_WIDTH - BALL_SIZE / 2;
                ball.dx = -Math.abs(ball.dx);
                ball.dy += offset * 0.005;
                clampBallSpeed(state);
            }
        }

        if (ball.x < 0) {
            state.score2 += 1;
            if (state.score2 >= WIN_SCORE) {
                state.status = 'finished';
                state.winner = 2;
            } else {
                resetBall(state, 1);
            }
        } else if (ball.x > 1) {
            state.score1 += 1;
            if (state.score1 >= WIN_SCORE) {
                state.status = 'finished';
                state.winner = 1;
            } else {
                resetBall(state, -1);
            }
        }

        updateScore(state);
        if (state.status === 'finished') showWinner(state.winner);
    }

    function startAI() {
        setControlHint();
        gameState = createAIState();
        statusElement.textContent = 'Playing vs Computer — use W/S or touch buttons.';
    }

    function render(state) {
        canvas.dataset.playerPaddleY = state.p1 ? String(state.p1.y) : '';
        ctx.clearRect(0, 0, W, H);

        ctx.strokeStyle = '#333';
        ctx.lineWidth = 2;
        ctx.setLineDash([8, 8]);
        ctx.beginPath();
        ctx.moveTo(W / 2, 0);
        ctx.lineTo(W / 2, H);
        ctx.stroke();
        ctx.setLineDash([]);

        const paddleHeight = PADDLE_HEIGHT;
        ctx.fillStyle = '#6366f1';
        const p1Y = (state.p1 ? state.p1.y : 0.5) * H;
        const p2Y = (state.p2 ? state.p2.y : 0.5) * H;
        ctx.fillRect(0, p1Y - (paddleHeight * H) / 2, PADDLE_WIDTH * W, paddleHeight * H);
        ctx.fillRect((1 - PADDLE_WIDTH) * W, p2Y - (paddleHeight * H) / 2, PADDLE_WIDTH * W, paddleHeight * H);

        if (state.ball) {
            const ballSize = BALL_SIZE * W;
            ctx.fillStyle = '#fff';
            ctx.beginPath();
            ctx.arc(state.ball.x * W, state.ball.y * H, ballSize / 2, 0, Math.PI * 2);
            ctx.fill();
        }

        ctx.fillStyle = '#555';
        ctx.font = '12px monospace';
        ctx.textAlign = 'center';
        ctx.fillText(isAI ? 'YOU (W/S)' : 'P1 (W/S)', 58, H - 10);
        ctx.fillText(isAI ? 'PC' : 'P2 (↑/↓)', W - 52, H - 10);

        if (state.status === 'waiting') {
            ctx.fillStyle = 'rgba(255,255,255,0.7)';
            ctx.font = '18px sans-serif';
            ctx.fillText('Waiting for players...', W / 2, H / 2 - 20);
        }
    }

    function interpolatedState(now) {
        if (stateBuffer.length === 0) return lastRenderedState;

        const target = now - interpolationDelay;
        let older = stateBuffer[0];
        let newer = stateBuffer[stateBuffer.length - 1];
        for (let index = 1; index < stateBuffer.length; index += 1) {
            if (stateBuffer[index].receivedAt >= target) {
                newer = stateBuffer[index];
                older = stateBuffer[index - 1];
                break;
            }
        }

        const span = newer.receivedAt - older.receivedAt;
        const amount = span > 0
            ? Math.max(0, Math.min(1, (target - older.receivedAt) / span))
            : 1;
        const lerp = (a, b) => a + (b - a) * amount;
        const a = older.state;
        const b = newer.state;
        lastRenderedState = {
            ...b,
            ball: { ...b.ball, x: lerp(a.ball.x, b.ball.x), y: lerp(a.ball.y, b.ball.y) },
            p1: { y: lerp(a.p1.y, b.p1.y) },
            p2: { y: lerp(a.p2.y, b.p2.y) },
        };
        return lastRenderedState;
    }

    let previousFrame = performance.now();
    let aiAccumulator = 0;

    function renderLoop(now) {
        const elapsed = Math.min(100, Math.max(0, now - previousFrame));
        previousFrame = now;

        if (isAI && gameState && gameState.status === 'playing') {
            aiAccumulator += elapsed;
            while (aiAccumulator >= TICK_MS) {
                tickAI();
                aiAccumulator -= TICK_MS;
            }
        }

        const state = isAI ? gameState : interpolatedState(now);
        if (state) {
            render(state);
            updateScore(state);
        }
        window.requestAnimationFrame(renderLoop);
    }

    lastRenderedState = {
        ball: { x: 0.5, y: 0.5 },
        p1: { y: 0.5 },
        p2: { y: 0.5 },
        score1: 0,
        score2: 0,
        status: 'waiting',
        winner: 0,
        p1_ready: false,
        p2_ready: false,
    };

    setControlHint();
    if (isAI) startAI();
    else connect();
    window.requestAnimationFrame(renderLoop);
})();
