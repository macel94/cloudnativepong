// Client-side PONG: renders game state from server, sends paddle input via WebSocket.
(function () {
    const params = new URLSearchParams(window.location.search);
    const roomId = params.get('room');
    const playerName = params.get('name') || 'Player';
    const mode = params.get('mode') || 'local';

    if (!roomId) {
        document.body.innerHTML = '<h1 style="text-align:center;margin-top:80px">Missing room ID</h1>';
        return;
    }

    document.getElementById('roomLabel').textContent = 'Room: ' + roomId;

    let ws;
    let player = 0; // 1 or 2, assigned by server
    let gameState = null;

    // Canvas setup
    const canvas = document.getElementById('pongCanvas');
    const ctx = canvas.getContext('2d');
    const W = canvas.width;
    const H = canvas.height;

    // Build WebSocket URL
    // Both local and gateway modes use the gateway path: /rooms/{id}/ws
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    let wsURL;
    if (mode === 'local') {
        wsURL = protocol + '//' + window.location.host + '/rooms/' + roomId + '/ws';
    } else {
        // In K8s mode, the gateway routes /rooms/{id}/ws to the correct room pod.
        wsURL = protocol + '//' + window.location.host + '/rooms/' + roomId + '/ws';
    }

    function connect() {
        ws = new WebSocket(wsURL);

        ws.onopen = function () {
            document.getElementById('status').textContent = 'Connected. Waiting for opponent...';
        };

        ws.onmessage = function (e) {
            const msg = JSON.parse(e.data);

            if (msg.type === 'joined') {
                player = msg.player;
                document.getElementById('status').textContent =
                    'You are Player ' + player + '. ' +
                    (player === 1 ? 'Use W/S to move.' : 'Use ↑/↓ to move.');
                if (player === 1) {
                    document.getElementById('status').textContent += ' Waiting for opponent...';
                }
            }

            if (msg.type === 'state') {
                gameState = msg.state;
                render(msg.state);

                if (msg.state.status === 'playing') {
                    document.getElementById('status').textContent = 'Playing!';
                }
                if (msg.state.status === 'finished') {
                    const winner = msg.state.winner;
                    const text = winner === player ? '🎉 You Win!' : '💀 You Lose!';
                    document.getElementById('winnerText').textContent = text;
                    document.getElementById('winnerOverlay').classList.remove('hidden');
                    document.getElementById('status').textContent = 'Game Over';
                    if (ws) ws.close();
                }

                document.getElementById('score1').textContent = msg.state.score1;
                document.getElementById('score2').textContent = msg.state.score2;
            }

            if (msg.type === 'error') {
                document.getElementById('status').textContent = 'Error: ' + msg.message;
            }
        };

        ws.onclose = function () {
            document.getElementById('status').textContent = 'Disconnected.';
        };

        ws.onerror = function () {
            document.getElementById('status').textContent = 'Connection error. Retrying...';
        };
    }

    // Keyboard input
    const keys = {};
    document.addEventListener('keydown', function (e) {
        keys[e.key] = true;
        e.preventDefault();
    });
    document.addEventListener('keyup', function (e) {
        keys[e.key] = false;
        e.preventDefault();
    });

    // Send input to server at ~60Hz
    setInterval(function () {
        if (!ws || ws.readyState !== WebSocket.OPEN || !player) return;

        let up = false, down = false;
        if (player === 1) {
            up = keys['w'] || keys['W'];
            down = keys['s'] || keys['S'];
        } else {
            up = keys['ArrowUp'];
            down = keys['ArrowDown'];
        }

        ws.send(JSON.stringify({
            player: player,
            up: up,
            down: down,
        }));
    }, 16);

    // Render the game state on canvas
    function render(state) {
        ctx.clearRect(0, 0, W, H);

        // Center line
        ctx.strokeStyle = '#333';
        ctx.lineWidth = 2;
        ctx.setLineDash([8, 8]);
        ctx.beginPath();
        ctx.moveTo(W / 2, 0);
        ctx.lineTo(W / 2, H);
        ctx.stroke();
        ctx.setLineDash([]);

        // Paddles
        const pw = state.ball ? 0.02 : 0.02;
        const ph = 0.15;
        ctx.fillStyle = '#6366f1';
        // Player 1 (left)
        const p1X = 0;
        const p1Y = (state.p1 ? state.p1.y : 0.5) * H;
        ctx.fillRect(p1X * W, p1Y - (ph * H) / 2, pw * W, ph * H);
        // Player 2 (right)
        const p2X = 1 - pw;
        const p2Y = (state.p2 ? state.p2.y : 0.5) * H;
        ctx.fillRect(p2X * W, p2Y - (ph * H) / 2, pw * W, ph * H);

        // Ball
        if (state.ball) {
            const ballSize = 0.025 * W;
            ctx.fillStyle = '#fff';
            ctx.beginPath();
            ctx.arc(state.ball.x * W, state.ball.y * H, ballSize / 2, 0, Math.PI * 2);
            ctx.fill();
        }

        // Player labels
        ctx.fillStyle = '#555';
        ctx.font = '12px monospace';
        ctx.textAlign = 'center';
        ctx.fillText('P1 (W/S)', 50, H - 10);
        ctx.fillText('P2 (↑/↓)', W - 50, H - 10);

        // Ready status
        if (state.status === 'waiting') {
            ctx.fillStyle = 'rgba(255,255,255,0.7)';
            ctx.font = '18px sans-serif';
            ctx.fillText('Waiting for players...', W / 2, H / 2 - 20);
        }
    }

    // Initial render
    render({
        ball: { x: 0.5, y: 0.5 },
        p1: { y: 0.5 },
        p2: { y: 0.5 },
        score1: 0, score2: 0,
        status: 'waiting', winner: 0,
        p1_ready: false, p2_ready: false,
    });

    connect();
})();