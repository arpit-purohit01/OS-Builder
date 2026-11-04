# Foundry Usability Test Runbook

## Overview

This runbook defines the standardized process for conducting usability tests with non-technical pilot users. The goal is to validate that a non-technical user can design, build, and install an OS unaided in under 30 minutes.

## Test Setup

### Prerequisites
- [ ] Foundry platform deployed and accessible
- [ ] Test device(s) available (matching pilot hardware)
- [ ] USB drives for installer media
- [ ] Windows evaluation image for dual-boot testing
- [ ] Screen recording software
- [ ] Consent forms signed

### Test Environment
- Quiet room with desk and chair
- Test device connected to power and network
- Second screen for observer notes
- USB drive with latest installer image
- Windows VM/device for dual-boot testing

## Participant Recruitment

### Target Profile
- Non-technical user (no sysadmin/DevOps experience)
- Typical pilot user persona (office admin, teacher, receptionist)
- Mix of ages and backgrounds
- No prior Foundry exposure

### Recruitment Script
> "We're testing a new tool that lets organizations create their own custom operating system without writing code. We'd like you to try building and installing an OS for a specific use case. The session will take about 2 hours and we'll record your screen and voice for analysis."

## Test Scenarios

### Scenario 1: Create a Kiosk OS (Target: 5 min)
**Persona:** Receptionist setting up lobby display
**Template:** Kiosk
**Goal:** Build a single-app kiosk OS for lobby display

**Steps:**
1. Open builder, select "Kiosk" template
2. Configure kiosk URL and idle reset time
3. Set security level to "hardened"
4. Choose TPM2 disk encryption
5. Save and build the OS
5. Download the ISO image

**Success Criteria:**
- [ ] Template selected correctly
- [ ] Kiosk URL configured
- [ ] Security settings applied
- [ ] Build completes successfully
- [ ] ISO downloaded

### Scenario 2: Create an Office Laptop OS (Target: 10 min)
**Persona:** IT admin setting up front-office laptops
**Template:** Office laptop
**Goal:** Build an office laptop OS with SSO, app store, and managed updates

**Steps:**
1. Open builder, select "Office laptop" template
2. Configure SSO/OIDC identity provider
3. Enable app store and select apps (Firefox, LibreOffice)
4. Configure disk encryption with TPM2+PIN
5. Set up fleet management enrollment
6. Customize branding (OS name, colors, logo)
7. Save and build the OS

**Success Criteria:**
- [ ] Template selected correctly
- [ ] SSO configured with issuer and client ID
- [ ] Apps selected and added
- [ ] Encryption configured with PIN
- [ ] Fleet enrollment enabled
- [ ] Branding applied
- [ ] Build completes successfully

### Scenario 3: Create a School Lab OS (Target: 10 min)
**Persona:** School IT coordinator
**Template:** School lab
**Goal:** Build a school computer lab OS with printing, locked-down access, and fleet management

**Steps:**
1. Open builder, select "School lab" template
2. Configure printing system (CUPS)
3. Set security level to "balanced"
4. Configure USB storage as read-only
5. Set up fleet management with device group
5. Disable admin access (locked root model)
6. Save and build the OS

**Success Criteria:**
- [ ] Template selected correctly
- [ ] Printing configured
- [ ] USB set to read-only
- [ ] Fleet enrollment configured
- [ ] Root access locked
- [ ] Build completes successfully

### Scenario 4: Custom Desktop Layout (Target: 3 min)
**Persona:** Designer customizing desktop
**Goal:** Design a custom panel layout with favorite apps

**Steps:**
1. Open the Layout Editor
2. Add a new panel at the bottom
3. Configure as dock style
4. Add favorite apps to the dock
5. Set panel size and autohide
6. Save and apply layout

**Success Criteria:**
- [ ] Layout editor opened
- [ ] Panel added and configured
- [ ] Favorite apps added to dock
- [ ] Layout saved and applied

### Scenario 5: Dual-Boot Install (Target: 20 min)
**Persona:** User installing alongside Windows
**Goal:** Install the OS alongside Windows using the installer

**Steps:**
1. Boot from USB installer media
2. Select "Install alongside Windows"
3. Confirm BitLocker recovery key availability
4. Select target disk partition
5. Configure user account
6. Complete installation and reboot
7. Verify dual-boot menu appears

**Success Criteria:**
- [ ] Installer boots correctly
- [ ] "Alongside Windows" option selected
- [ ] BitLocker recovery key confirmed
- [ ] Installation completes
- [ ] Dual-boot menu works
- [ ] Both OSes boot successfully

## Observation Checklist

### For Each Task
- [ ] Task started
- [ ] Time started
- [ ] Task completed successfully?
- [ ] Time completed
- [ ] Errors observed (list)
- [ ] User got stuck?
- [ ] UI confusing?
- [ ] Wrong path taken?
- [ ] Difficulty rating (1-5)
- [ ] Observer notes

### Overall Session
- [ ] Total session time
- [ ] Tasks passed / total
- [ ] Average difficulty rating
- [ ] Participant overall rating (1-5)
- [ ] Participant verbal feedback
- [ ] Critical issues found
- [ ] Suggestions for improvement

## Data Recording

### Session Record
```json
{
  "sessionId": "test-2024-01-15-001",
  "participantId": "p-001",
  "projectId": "prj_test",
  "startTime": "2024-01-15T10:00:00Z",
  "endTime": "2024-01-15T11:30:00Z",
  "participantProfile": {
    "role": "receptionist",
    "technicalExperience": "none",
    "ageRange": "35-44"
  },
  "tasks": [
    {
      "taskId": "create-kiosk-0",
      "taskName": "Open builder, select Kiosk template",
      "success": true,
      "timeSeconds": 45,
      "errors": [],
      "difficulty": 1,
      "comments": "Found template immediately"
    }
  ],
  "overallRating": 4,
  "notes": "Participant found the template selection intuitive but hesitated on security settings."
}
```

## Analysis & Reporting

### Metrics to Track
1. **Task Success Rate** = Passed Tasks / Total Tasks (target: ≥ 80%)
2. **Time on Task** = Median time per task vs expected
3. **Error Rate** = Errors per task
4. **Difficulty Rating** = Average 1-5 (target: ≤ 3)
4. **Completion Rate** = Sessions with all tasks passed / Total sessions
5. **SUS Score** (optional) = System Usability Scale

### Success Thresholds (Phase 2 Exit Gate)
- [ ] Usability goal met by 4 of 5 participants
- [ ] Installer success on all pilot device models
- [ ] Median task time ≤ expected time × 1.5
- [ ] Zero critical blockers (user completely unable to proceed)

### Report Template
```markdown
# Usability Test Report - [Date]

## Summary
- Participants: 5
- Scenarios tested: 5
- Overall success rate: X%

## Key Findings
### Critical Issues
1. [Issue description] - Affected X/5 users
2. ...

### Usability Issues
1. [Issue description] - Affected X/5 users
2. ...

### Positive Feedback
1. [What worked well]

## Recommendations
1. [Priority 1 - Blocking]
2. [Priority 2 - Major]
3. [Priority 3 - Minor]

## Participant Quotes
> "Quote from participant"
```

## Issue Triage

### Priority Levels
- **P0 - Blocker**: User completely unable to complete task
- **P1 - Major**: Significant confusion, workaround needed
- **P2 - Minor**: Minor confusion, recoverable
- **P3 - Cosmetic**: Visual/wording improvements

### Triage Process
1. Collect all issues from all sessions
2. Deduplicate similar issues
3. Assign priority
4. Assign owner
4. Set target fix version
5. Track in issue tracker

## Follow-up
- [ ] Issues filed in tracker
- [ ] Fixes implemented
- [ ] Regression test with same scenarios
- [ ] Sign-off from product/design

## Appendix: Observer Script

> "Welcome! We're testing a new tool for creating custom operating systems. There are no right or wrong answers - we're testing the tool, not you. Please think aloud as you work. If you get stuck, just tell me what you're thinking. I won't help unless you're completely stuck. Any questions before we start?"

### During Test
- Observe silently
- Take timestamped notes
- Only intervene if:
  - Participant asks for help
  - 5+ minutes with no progress
  - Safety concern
- Use prompts:
  - "What are you thinking?"
  - "What did you expect to happen?"
  - "What would you do next?"

### Post-Test Interview
1. "How would you rate the overall experience? (1-5)"
2. "What was the most confusing part?"
3. "What worked really well?"
4. "If you could change one thing, what would it be?"
5. "Would you recommend this to a colleague?"