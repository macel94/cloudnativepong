// Cloud Native Pong client: online WebSocket play, local heuristic AI, and touch controls.
(function () {
    'use strict';

    const params = new URLSearchParams(window.location.search);
    const roomId = params.get('room');
    const playerName = params.get('name') || 'Player';
    const mode = params.get('mode') || 'local';
    const isAI = mode === 'ai';
    const isSpectator = mode === 'spectator';

    const roomLabel = document.getElementById('roomLabel');
    const statusElement = document.getElementById('status');
    const controlHint = document.getElementById('controlHint');
    const touchControls = document.getElementById('touchControls');
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

    document.body.dataset.gameMode = isAI ? 'ai' : (isSpectator ? 'spectator' : 'online');
    roomLabel.textContent = isAI
        ? 'vs Computer · Local AI'
        : (isSpectator ? 'Room: ' + roomId + ' · Live spectator' : 'Room: ' + roomId);
    if (isSpectator && touchControls) touchControls.classList.add('hidden');

    let ws = null;
    let connection = null;
    let player = isAI ? 1 : 0;
    let gameState = null;
    let gameOverShown = false;
    let lastRenderedState = null;
    let predictedLocalPaddleY = null;
    let reconciliationTargetY = null;
    let nextInputSequence = 0;
    let lastAcknowledgedInputSequence = 0;
    const pendingInputs = [];

    // Keep only recent snapshots for velocity estimation. Online rendering
    // uses the newest snapshot immediately and dead-reckons between packets;
    // it never intentionally renders 50ms in the past.
    const stateBuffer = [];
    const maxExtrapolationMs = 120;
    const correctionDecayMs = 80;
    let presentationCorrection = {
        ball: { x: 0, y: 0 },
        p1: 0,
        p2: 0,
    };
    let presentationCorrectionAt = performance.now();

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
        if (isSpectator) {
            controlHint.textContent = 'Watching live · controls disabled';
            return;
        }
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
        winnerText.textContent = isSpectator
            ? `Player ${winner} wins!`
            : (winner === player ? '🎉 You Win!' : '💀 You Lose!');
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
        if (isSpectator || !movementKeys.has(event.key)) return;
        event.preventDefault();
        keys[event.key] = true;
    });
    document.addEventListener('keyup', (event) => {
        if (isSpectator || !movementKeys.has(event.key)) return;
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

    function localPaddleKey() {
        return player === 2 ? 'p2' : 'p1';
    }

    function localInputSequenceKey() {
        return player === 2 ? 'p2_input_sequence' : 'p1_input_sequence';
    }

    function localPaddleY(state) {
        const paddle = state && state[localPaddleKey()];
        return paddle && Number.isFinite(paddle.y) ? paddle.y : 0.5;
    }

    function inputSequence(state) {
        const value = state && state[localInputSequenceKey()];
        return Number.isSafeInteger(value) && value >= 0 ? value : 0;
    }

    function applyPaddleInput(y, input, elapsedMs) {
        const ticks = Math.min(8, Math.max(0, elapsedMs) / TICK_MS);
        if (input.up) y -= PADDLE_SPEED * ticks;
        if (input.down) y += PADDLE_SPEED * ticks;
        return clampPaddle(y);
    }

    function reconcileLocalPaddle(state, receivedAt) {
        const authoritativeY = localPaddleY(state);
        const acknowledged = inputSequence(state);
        let acknowledgedSentAt = 0;
        if (acknowledged > lastAcknowledgedInputSequence) {
            lastAcknowledgedInputSequence = acknowledged;
            for (let index = pendingInputs.length - 1; index >= 0; index -= 1) {
                if (pendingInputs[index].sequence <= acknowledged) {
                    acknowledgedSentAt = pendingInputs[index].sentAt;
                    break;
                }
            }
            while (pendingInputs.length > 0 && pendingInputs[0].sequence <= acknowledged) {
                pendingInputs.shift();
            }
        }

        // The server state is historical by the time it is received. Forecast
        // the currently held intent through one estimated one-way delay plus
        // one authoritative tick, then ease the visible paddle toward that
        // point. This is reconciliation, not authority: the next snapshot can
        // always correct the presentation if the prediction was wrong.
        const roundTrip = acknowledgedSentAt > 0
            ? Math.max(0, receivedAt - acknowledgedSentAt)
            : 100;
        const oneWay = Math.min(150, roundTrip / 2);
        reconciliationTargetY = applyPaddleInput(
            authoritativeY,
            readLocalInput(),
            oneWay + TICK_MS,
        );

        if (predictedLocalPaddleY === null) {
            predictedLocalPaddleY = reconciliationTargetY;
        }
    }

    function updatePredictedLocalPaddle(elapsedMs) {
        if (!gameState || gameState.status !== 'playing' || !player || predictedLocalPaddleY === null) return;
        predictedLocalPaddleY = applyPaddleInput(
            predictedLocalPaddleY,
            readLocalInput(),
            elapsedMs,
        );
        if (reconciliationTargetY !== null) {
            // A 100 ms correction window hides packet jitter while keeping a
            // real server correction bounded and observable to the player.
            const correction = Math.min(1, Math.max(0, elapsedMs) / 100);
            predictedLocalPaddleY = clampPaddle(
                predictedLocalPaddleY + (reconciliationTargetY - predictedLocalPaddleY) * correction,
            );
        }
    }

    function handleMessage(message) {
        if (message.type === 'spectator') {
            player = 0;
            stateBuffer.length = 0;
            predictedLocalPaddleY = null;
            reconciliationTargetY = null;
            setControlHint();
            statusElement.textContent = 'Spectating live game. Waiting for players...';
        }

        if (message.type === 'joined') {
            player = message.player;
            stateBuffer.length = 0;
            presentationCorrection = {
                ball: { x: 0, y: 0 },
                p1: 0,
                p2: 0,
            };
            presentationCorrectionAt = performance.now();
            nextInputSequence = 0;
            predictedLocalPaddleY = null;
            reconciliationTargetY = null;
            lastAcknowledgedInputSequence = 0;
            pendingInputs.length = 0;
            setControlHint();
            statusElement.textContent =
                'You are Player ' + player + '. ' +
                (player === 1 ? 'Use W/S or touch buttons.' : 'Use ↑/↓ or touch buttons.') +
                (player === 1 ? ' Waiting for opponent...' : '');
        }

        if (message.type === 'state' && message.state && message.state.ball) {
            const state = message.state;
            const receivedAt = performance.now();
            updatePresentationCorrection(lastRenderedState, state);
            gameState = state;
            if (!isSpectator) reconcileLocalPaddle(state, receivedAt);
            stateBuffer.push({ state, receivedAt });
            while (stateBuffer.length > 4 ||
                (stateBuffer.length > 2 && receivedAt - stateBuffer[0].receivedAt > 160)) {
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
        const spectatorQuery = isSpectator ? '?spectator=1' : '';
        ws = new WebSocket(wsURL + spectatorQuery);
        connection = {
            send: (value) => ws.send(JSON.stringify(value)),
            close: () => ws.close(),
            isOpen: () => ws.readyState === WebSocket.OPEN,
        };

        ws.onopen = function () {
            statusElement.textContent = isSpectator
                ? 'Connected via WebSocket. Joining spectator view...'
                : 'Connected via WebSocket. Waiting for opponent...';
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
                    const spectatorQuery = isSpectator ? (url.includes('?') ? '&' : '?') + 'spectator=1' : '';
                    await connectWebTransport(url + spectatorQuery);
                    return;
                }
            } catch {
                // Capability discovery or QUIC setup failed; use the tested
                // WebSocket fallback without delaying the game unnecessarily.
            }
        }
        connectWebSocket();
    }

    // Send input to the authoritative server at approximately 60Hz. The
    // sequence is an acknowledgement marker, not a movement command: the
    // server remains authoritative and echoes the latest sequence it applied.
    setInterval(() => {
        if (isAI || isSpectator || !connection || !connection.isOpen() || !player) return;
        const input = readLocalInput();
        const sequence = ++nextInputSequence;
        pendingInputs.push({ sequence, sentAt: performance.now() });
        while (pendingInputs.length > 128) pendingInputs.shift();
        connection.send({ player, up: input.up, down: input.down, sequence });
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
        canvas.dataset.ballX = state.ball ? String(state.ball.x) : '';
        canvas.dataset.ballY = state.ball ? String(state.ball.y) : '';
        canvas.dataset.opponentPaddleY = player === 1
            ? (state.p2 ? String(state.p2.y) : '')
            : (state.p1 ? String(state.p1.y) : '');
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

    function clampUnit(value) {
        return Math.max(0, Math.min(1, value));
    }

    function reflectBallY(value, direction) {
        let y = value;
        let dy = direction;
        while (y < BALL_SIZE / 2 || y > 1 - BALL_SIZE / 2) {
            if (y < BALL_SIZE / 2) {
                y = BALL_SIZE - y;
                dy = Math.abs(dy);
            } else if (y > 1 - BALL_SIZE / 2) {
                y = 2 - BALL_SIZE - y;
                dy = -Math.abs(dy);
            }
        }
        return { y, dy };
    }

    function snapshotVelocity(older, newer, key) {
        const span = newer.receivedAt - older.receivedAt;
        if (span <= 0) return { x: 0, y: 0 };
        const first = older.state[key];
        const second = newer.state[key];
        return {
            x: (second.x - first.x) / span,
            y: (second.y - first.y) / span,
        };
    }

    function paddleVelocity(older, newer, key) {
        const span = newer.receivedAt - older.receivedAt;
        if (span <= 0) return 0;
        return (newer.state[key].y - older.state[key].y) / span;
    }

    function decayPresentationCorrection(now) {
        const elapsed = Math.max(0, now - presentationCorrectionAt);
        const amount = Math.exp(-elapsed / correctionDecayMs);
        presentationCorrectionAt = now;
        presentationCorrection.ball.x *= amount;
        presentationCorrection.ball.y *= amount;
        presentationCorrection.p1 *= amount;
        presentationCorrection.p2 *= amount;
    }

    function extrapolatedState(now) {
        if (stateBuffer.length === 0) return lastRenderedState;

        const newest = stateBuffer[stateBuffer.length - 1];
        const older = stateBuffer.length > 1
            ? stateBuffer[stateBuffer.length - 2]
            : newest;
        const elapsedMs = Math.min(maxExtrapolationMs, Math.max(0, now - newest.receivedAt));
        // Ball velocity is part of the authoritative state, so it can be
        // rendered immediately even before a second snapshot arrives. The
        // snapshot-derived remote-paddle velocity is only used where the wire
        // protocol does not carry paddle velocity.
        const ballVelocity = {
            x: newest.state.ball.dx / TICK_MS,
            y: newest.state.ball.dy / TICK_MS,
        };
        const p1Velocity = paddleVelocity(older, newest, 'p1');
        const p2Velocity = paddleVelocity(older, newest, 'p2');
        const ballX = newest.state.ball.x + ballVelocity.x * elapsedMs;
        const rawBallY = newest.state.ball.y + ballVelocity.y * elapsedMs;
        const reflected = reflectBallY(rawBallY, newest.state.ball.dy);
        const localPaddle = localPaddleKey();

        decayPresentationCorrection(now);
        const state = {
            ...newest.state,
            ball: {
                ...newest.state.ball,
                x: clampUnit(ballX + presentationCorrection.ball.x),
                y: clampUnit(reflected.y + presentationCorrection.ball.y),
                dy: reflected.dy,
            },
            p1: { y: localPaddle === 'p1' && predictedLocalPaddleY !== null
                ? predictedLocalPaddleY
                : clampPaddle(newest.state.p1.y + p1Velocity * elapsedMs + presentationCorrection.p1) },
            p2: { y: localPaddle === 'p2' && predictedLocalPaddleY !== null
                ? predictedLocalPaddleY
                : clampPaddle(newest.state.p2.y + p2Velocity * elapsedMs + presentationCorrection.p2) },
        };
        lastRenderedState = state;
        return state;
    }

    function updatePresentationCorrection(previous, next) {
        if (!previous || !next) return;
        presentationCorrection.ball.x += previous.ball.x - next.ball.x;
        presentationCorrection.ball.y += previous.ball.y - next.ball.y;
        presentationCorrection.p1 += previous.p1.y - next.p1.y;
        presentationCorrection.p2 += previous.p2.y - next.p2.y;
        presentationCorrection.ball.x = Math.max(-0.2, Math.min(0.2, presentationCorrection.ball.x));
        presentationCorrection.ball.y = Math.max(-0.2, Math.min(0.2, presentationCorrection.ball.y));
        presentationCorrection.p1 = Math.max(-0.2, Math.min(0.2, presentationCorrection.p1));
        presentationCorrection.p2 = Math.max(-0.2, Math.min(0.2, presentationCorrection.p2));
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
        } else if (!isAI) {
            updatePredictedLocalPaddle(elapsed);
        }

        const state = isAI ? gameState : extrapolatedState(now);
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
