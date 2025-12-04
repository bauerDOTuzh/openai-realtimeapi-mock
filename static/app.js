document.addEventListener('DOMContentLoaded', () => {
    const configDisplay = document.getElementById('config-display');
    const scenariosList = document.getElementById('scenarios-list');
    const recordingsList = document.getElementById('recordings-list');
    const refreshRecordingsBtn = document.getElementById('refresh-recordings');
    const connectionStatus = document.getElementById('connection-status');
    const recordingViewer = document.getElementById('recording-viewer');
    const viewerTitle = document.getElementById('viewer-title');
    const viewerContent = document.getElementById('viewer-content');
    const closeViewerBtn = document.getElementById('close-viewer');

    // Helper to format JSON
    function syntaxHighlight(json) {
        if (typeof json !== 'string') {
            json = JSON.stringify(json, undefined, 2);
        }
        json = json.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
        return json.replace(/("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g, function (match) {
            var cls = 'json-number';
            if (/^"/.test(match)) {
                if (/:$/.test(match)) {
                    cls = 'json-key';
                } else {
                    cls = 'json-string';
                }
            } else if (/true|false/.test(match)) {
                cls = 'json-boolean';
            } else if (/null/.test(match)) {
                cls = 'json-null';
            }
            return '<span class="' + cls + '">' + match + '</span>';
        });
    }

    async function fetchConfig() {
        try {
            const res = await fetch('/config');
            if (!res.ok) throw new Error('Failed to fetch config');
            const config = await res.json();

            renderConfig(config);
            renderScenarios(config.scenarios);
            updateStatus(true);
        } catch (err) {
            console.error(err);
            configDisplay.innerHTML = '<span class="text-red-500">Error loading config</span>';
            updateStatus(false);
        }
    }

    function renderConfig(config) {
        let html = `
            <div class="flex justify-between"><span class="text-gray-400">Mode:</span> <span class="font-mono text-white bg-gray-700 px-1 rounded">${config.mode}</span></div>
            <div class="flex justify-between"><span class="text-gray-400">Port:</span> <span class="font-mono text-white">${config.server.port}</span></div>
        `;

        if (config.mode === 'proxy') {
            html += `
                <div class="flex justify-between"><span class="text-gray-400">Target:</span> <span class="font-mono text-white truncate w-32" title="${config.proxy.url}">${config.proxy.url}</span></div>
                <div class="flex justify-between"><span class="text-gray-400">Model:</span> <span class="font-mono text-white">${config.proxy.model}</span></div>
            `;
        } else {
            html += `
                <div class="flex justify-between"><span class="text-gray-400">Delay:</span> <span class="font-mono text-white">${config.mock.responseDelaySeconds}s</span></div>
            `;
        }
        configDisplay.innerHTML = html;
    }

    function renderScenarios(scenarios) {
        if (!scenarios || scenarios.length === 0) {
            scenariosList.innerHTML = '<li class="text-gray-500 italic">No scenarios defined</li>';
            return;
        }

        scenariosList.innerHTML = scenarios.map(s => `
            <li class="bg-gray-700 p-2 rounded hover:bg-gray-600 transition cursor-default">
                <div class="font-semibold text-blue-300">${s.name}</div>
                <div class="text-xs text-gray-400">${s.events ? s.events.length : 0} events</div>
            </li>
        `).join('');
    }

    async function fetchRecordings() {
        try {
            const res = await fetch('/recordings');
            if (!res.ok) throw new Error('Failed to fetch recordings');
            const recordings = await res.json();
            renderRecordings(recordings);
        } catch (err) {
            console.error(err);
            recordingsList.innerHTML = '<tr><td colspan="3" class="py-2 text-center text-red-500">Error loading recordings</td></tr>';
        }
    }

    function renderRecordings(recordings) {
        if (!recordings || recordings.length === 0) {
            recordingsList.innerHTML = '<tr><td colspan="3" class="py-2 text-center text-gray-500 italic">No recordings found</td></tr>';
            return;
        }

        // Sort by name (assuming timestamped names) descending
        recordings.sort((a, b) => b.name.localeCompare(a.name));

        recordingsList.innerHTML = recordings.map(r => `
            <tr class="hover:bg-gray-700 transition">
                <td class="py-2 font-mono text-xs text-gray-300 truncate max-w-xs" title="${r.name}">${r.name}</td>
                <td class="py-2 text-right text-xs text-gray-400">${formatBytes(r.size)}</td>
                <td class="py-2 text-right">
                    <button class="text-xs bg-blue-600 hover:bg-blue-500 text-white px-2 py-1 rounded view-recording-btn" data-name="${r.name}">View</button>
                    <button class="text-xs bg-green-600 hover:bg-green-500 text-white px-2 py-1 rounded replay-btn ml-1" data-name="${r.name}">Replay</button>
                </td>
            </tr>
        `).join('');

        document.querySelectorAll('.view-recording-btn').forEach(btn => {
            btn.addEventListener('click', (e) => viewRecording(e.target.dataset.name));
        });

        document.querySelectorAll('.replay-btn').forEach(btn => {
            btn.addEventListener('click', (e) => replayRecording(e.target.dataset.name));
        });
    }

    async function viewRecording(name) {
        viewerTitle.textContent = `Recording: ${name}`;
        viewerContent.innerHTML = '<span class="text-gray-400">Loading...</span>';
        recordingViewer.classList.remove('hidden');

        try {
            const res = await fetch(`/recordings/${name}`);
            if (!res.ok) throw new Error('Failed to load recording');
            const text = await res.text();

            const events = [];
            const lines = text.trim().split('\n');
            for (const line of lines) {
                try {
                    if (line.trim()) {
                        events.push(JSON.parse(line));
                    }
                } catch (e) {
                    console.warn('Failed to parse line:', line);
                }
            }

            renderEvents(events);

            // Add toggle all button to header if not exists
            if (!document.getElementById('toggle-all-btn')) {
                const btn = document.createElement('button');
                btn.id = 'toggle-all-btn';
                btn.className = 'text-xs bg-gray-700 hover:bg-gray-600 text-white px-2 py-1 rounded ml-4';
                btn.textContent = 'Expand All';
                btn.onclick = toggleAllDetails;
                viewerTitle.appendChild(btn);
            }
        } catch (err) {
            viewerContent.innerHTML = `<span class="text-red-500">Error: ${err.message}</span>`;
        }
    }

    function renderEvents(events) {
        if (events.length === 0) {
            viewerContent.innerHTML = '<span class="text-gray-500 italic">Empty recording</span>';
            return;
        }

        let html = '<div class="space-y-0 divide-y divide-gray-700">';
        let currentGroup = null;

        events.forEach((event, index) => {
            const type = event.data?.type || 'unknown';
            const timestamp = event.timestamp || 0;
            const dateStr = new Date(timestamp).toLocaleTimeString() + '.' + (timestamp % 1000).toString().padStart(3, '0');

            // Determine if this event should be grouped
            // Group audio deltas and input audio appends
            const shouldGroup = type === 'response.audio.delta' || type === 'input_audio_buffer.append';

            if (shouldGroup) {
                if (currentGroup && currentGroup.type === type) {
                    currentGroup.count++;
                    currentGroup.endTime = dateStr;
                    return; // Continue grouping
                } else {
                    // Close previous group if exists
                    if (currentGroup) {
                        html += renderGroup(currentGroup);
                    }
                    // Start new group
                    currentGroup = {
                        type: type,
                        startTime: dateStr,
                        endTime: dateStr,
                        count: 1,
                        sampleEvent: event
                    };
                }
            } else {
                // Close previous group if exists
                if (currentGroup) {
                    html += renderGroup(currentGroup);
                    currentGroup = null;
                }
                // Render single event
                html += renderSingleEvent(event, dateStr, type);
            }
        });

        // Close final group
        if (currentGroup) {
            html += renderGroup(currentGroup);
        }

        html += '</div>';
        viewerContent.innerHTML = html;

        // Add event listeners for toggles
        document.querySelectorAll('.toggle-details').forEach(el => {
            el.addEventListener('click', function () {
                const details = this.nextElementSibling;
                const chevron = this.querySelector('.chevron');
                details.classList.toggle('hidden');
                if (details.classList.contains('hidden')) {
                    chevron.textContent = '▶';
                } else {
                    chevron.textContent = '▼';
                }
            });
        });
    }

    function toggleAllDetails() {
        const btn = document.getElementById('toggle-all-btn');
        const isExpanding = btn.textContent === 'Expand All';

        document.querySelectorAll('.event-details').forEach(el => {
            if (isExpanding) el.classList.remove('hidden');
            else el.classList.add('hidden');
        });

        document.querySelectorAll('.chevron').forEach(el => {
            el.textContent = isExpanding ? '▼' : '▶';
        });

        btn.textContent = isExpanding ? 'Collapse All' : 'Expand All';
    }

    function renderGroup(group) {
        return `<div class="p-0.5 text-xs border-b border-gray-800"><div class="flex justify-between items-center text-gray-400"><div class="flex items-center gap-2"><span class="font-mono text-blue-400">${group.startTime} - ${group.endTime}</span><span class="font-semibold text-yellow-300">${group.type}</span></div><span class="bg-gray-700 px-2 py-0.5 rounded text-gray-300">${group.count} events</span></div></div>`;
    }

    function renderSingleEvent(event, dateStr, type) {
        const eventId = event.data?.event_id || '';
        return `<div class="p-0.5 text-xs hover:bg-gray-800 transition event-item leading-tight border-b border-gray-800"><div class="flex justify-between items-center cursor-pointer toggle-details select-none"><div class="flex items-center gap-2"><span class="text-gray-500 text-[10px] w-3 chevron">▶</span><span class="font-mono text-blue-400">${dateStr}</span><span class="font-semibold text-green-400">${type}</span></div><span class="text-gray-500 font-mono text-[10px]">${eventId}</span></div><div class="hidden event-details border-t border-gray-700 pt-0.5 mt-0.5"><pre class="syntax-highlight overflow-x-auto text-gray-300">${syntaxHighlight(event.data)}</pre></div></div>`;
    }

    function replayRecording(name) {
        // Just show an alert for now, or maybe copy the replay URL
        const replayUrl = `ws://${window.location.host}/v1/realtime?replaySession=${name}`;
        alert(`To replay this session, connect to:\n${replayUrl}`);
    }

    function formatBytes(bytes, decimals = 2) {
        if (bytes === 0) return '0 Bytes';
        const k = 1024;
        const dm = decimals < 0 ? 0 : decimals;
        const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i];
    }

    function updateStatus(connected) {
        if (connected) {
            connectionStatus.textContent = 'Connected';
            connectionStatus.className = 'px-3 py-1 rounded-full text-xs font-semibold bg-green-900 text-green-200';
        } else {
            connectionStatus.textContent = 'Disconnected';
            connectionStatus.className = 'px-3 py-1 rounded-full text-xs font-semibold bg-red-900 text-red-200';
        }
    }

    closeViewerBtn.addEventListener('click', () => {
        recordingViewer.classList.add('hidden');
    });

    refreshRecordingsBtn.addEventListener('click', fetchRecordings);

    // Initial load
    fetchConfig();
    fetchRecordings();
});
