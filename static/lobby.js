// Lobby: room list, create/join rooms
const API = '/api/rooms';

let playerName = '';

document.addEventListener('DOMContentLoaded', () => {
    const nameInput = document.getElementById('playerName');
    const saved = localStorage.getItem('pong_player_name');
    if (saved) {
        nameInput.value = saved;
        playerName = saved;
    }

    nameInput.addEventListener('input', () => {
        playerName = nameInput.value.trim();
        localStorage.setItem('pong_player_name', playerName);
    });

    document.getElementById('btnNewRoom').addEventListener('click', createRoom);
    document.getElementById('btnAI').addEventListener('click', playAgainstComputer);
    refreshRooms();
    setInterval(refreshRooms, 3000);
});

async function refreshRooms() {
    try {
        const res = await fetch(API);
        const rooms = await res.json();
        renderRooms(rooms);
        document.getElementById('noRooms').classList.toggle('hidden', rooms.length > 0);
    } catch (e) {
        console.error('Failed to fetch rooms', e);
    }
}

function renderRooms(rooms) {
    const el = document.getElementById('rooms');
    el.innerHTML = rooms.map(r => `
        <div class="room-card">
            <div>
                <span class="room-name">${esc(r.name || 'Untitled')}</span>
                <span class="room-meta"> &middot; ${r.players}/2 players &middot; ${r.status}</span>
            </div>
            <div class="room-actions">
                <button class="btn primary join-btn" onclick="joinRoom('${r.id}')" ${r.players >= 2 ? 'disabled' : ''}>
                    ${r.players >= 2 ? 'Full' : 'Join'}
                </button>
                ${r.status === 'playing' && r.players >= 2
                    ? `<button class="btn secondary watch-btn" onclick="watchRoom('${r.id}')">Watch</button>`
                    : ''}
            </div>
        </div>
    `).join('');
}

function watchRoom(id) {
    window.location.href = '/game.html?room=' + encodeURIComponent(id) + '&mode=spectator';
}

function playAgainstComputer() {
    const name = playerName || 'Player';
    window.location.href = '/game.html?mode=ai&name=' + encodeURIComponent(name);
}

async function createRoom() {
    if (!playerName) {
        showError('Enter a display name first.');
        return;
    }
    try {
        const res = await fetch(API + '/create', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: playerName + "'s room" }),
        });
        const room = await res.json();
        if (room.id) {
            window.location.href = '/game.html?room=' + room.id + '&name=' + encodeURIComponent(playerName);
        }
    } catch (e) {
        showError('Failed to create room: ' + e.message);
    }
}

function joinRoom(id) {
    if (!playerName) {
        showError('Enter a display name first.');
        return;
    }
    fetch(API + '/join', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ room_id: id }),
    }).then(r => r.json()).then(data => {
        if (data.error) {
            showError(data.error);
            return;
        }
        window.location.href = '/game.html?room=' + id + '&name=' + encodeURIComponent(playerName) + '&mode=' + (data.mode || 'local');
    }).catch(e => showError(e.message));
}

function showError(msg) {
    const el = document.getElementById('error');
    el.textContent = msg;
    el.classList.remove('hidden');
    setTimeout(() => el.classList.add('hidden'), 4000);
}

function esc(s) {
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}