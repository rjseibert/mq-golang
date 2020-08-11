/*
Package mqmetric contains a set of routines common to several
commands used to export MQ metrics to different backend
storage mechanisms including Prometheus and InfluxDB.
*/
package mqmetric

/*
  Copyright (c) IBM Corporation 2018,2020

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.

   Contributors:
     Mark Taylor - Initial Contribution
*/

/*
Functions in this file use the DISPLAY QueueStatus command to extract metrics
about MQ queues
*/

import (
	//	"fmt"
	"github.com/ibm-messaging/mq-golang/v5/ibmmq"
	"strings"
	"time"
)

const (
	ATTR_Q_NAME        = "name"
	ATTR_Q_MSGAGE      = "oldest_message_age"
	ATTR_Q_IPPROCS     = "input_handles"
	ATTR_Q_OPPROCS     = "output_handles"
	ATTR_Q_QTIME_SHORT = "qtime_short"
	ATTR_Q_QTIME_LONG  = "qtime_long"
	ATTR_Q_DEPTH       = "depth"
	ATTR_Q_CURFSIZE    = "qfile_current_size"
	ATTR_Q_SINCE_PUT   = "time_since_put"
	ATTR_Q_SINCE_GET   = "time_since_get"
	ATTR_Q_MAX_DEPTH   = "attribute_max_depth"
	ATTR_Q_USAGE       = "attribute_usage"
	ATTR_Q_CURMAXFSIZE = "qfile_max_size"

	// The next two attributes are given the same name
	// as the published statistics from the amqsrua-style
	// vaues. That allows a dashboard for Distributed and z/OS
	// to merge the same query.
	ATTR_Q_INTERVAL_PUT = "mqput_mqput1_count"
	ATTR_Q_INTERVAL_GET = "mqget_count"
	// This is the Highest Depth returned over an interval via the
	// RESET QSTATS command. Contrast with the attribute_max_depth
	// value which is the DISPLAY QL(x) MAXDEPTH attribute.
	ATTR_Q_INTERVAL_HI_DEPTH = "hi_depth"
)

var QueueStatus StatusSet
var qAttrsInit = false

/*
Unlike the statistics produced via a topic, there is no discovery
of the attributes available in object STATUS queries. There is also
no discovery of descriptions for them. So this function hardcodes the
attributes we are going to look for and gives the associated descriptive
text. The elements can be expanded later; just trying to give a starting point
for now.
*/
func QueueInitAttributes() {
	traceEntry("QueueInitAttributes")
	if qAttrsInit {
		traceExit("QueueInitAttributes", 1)
		return
	}
	QueueStatus.Attributes = make(map[string]*StatusAttribute)

	attr := ATTR_Q_NAME
	QueueStatus.Attributes[attr] = newPseudoStatusAttribute(attr, "Queue Name")

	attr = ATTR_Q_SINCE_PUT
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Time Since Put", -1)
	attr = ATTR_Q_SINCE_GET
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Time Since Get", -1)

	// These are the integer status fields that are of interest
	attr = ATTR_Q_MSGAGE
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Oldest Message", ibmmq.MQIACF_OLDEST_MSG_AGE)
	attr = ATTR_Q_IPPROCS
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Input Handles", ibmmq.MQIA_OPEN_INPUT_COUNT)
	attr = ATTR_Q_OPPROCS
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Input Handles", ibmmq.MQIA_OPEN_OUTPUT_COUNT)

	// QFile sizes - current, and the "current maximum" which may not be
	// the same as the qdefinition but is the one in effect for now until
	// the qfile empties
	attr = ATTR_Q_CURFSIZE
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue File Current Size", ibmmq.MQIACF_CUR_Q_FILE_SIZE)
	attr = ATTR_Q_CURMAXFSIZE
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue File Maximum Size", ibmmq.MQIACF_CUR_MAX_FILE_SIZE)

	// Usually we get the QDepth from published resources, But on z/OS we can get it from the QSTATUS response
	if !usePublications {
		attr = ATTR_Q_DEPTH
		QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue Depth", ibmmq.MQIA_CURRENT_Q_DEPTH)
	}

	if platform == ibmmq.MQPL_ZOS && useResetQStats {
		attr = ATTR_Q_INTERVAL_PUT
		QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Put/Put1 Count", ibmmq.MQIA_MSG_ENQ_COUNT)
		attr = ATTR_Q_INTERVAL_GET
		QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Get Count", ibmmq.MQIA_MSG_DEQ_COUNT)
		attr = ATTR_Q_INTERVAL_HI_DEPTH
		QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Highest Depth", ibmmq.MQIA_HIGH_Q_DEPTH)
	}

	// This is not really a monitoring metric but it enables calculations to be made such as %full for
	// the queue. It's extracted at startup of the program via INQUIRE_Q and not updated later even if the
	// queue definition is changed until rediscovery of the queues on a schedule.
	// It's not easy to generate the % value in this program as the CurDepth will
	// usually - but not always - come from the published resource stats. So we don't have direct access to it.
	// Recording the MaxDepth allows Prometheus etc to do the calculation regardless of how the CurDepth was obtained.
	attr = ATTR_Q_MAX_DEPTH
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue Max Depth", -1)
	attr = ATTR_Q_USAGE
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue Usage", -1)

	attr = ATTR_Q_QTIME_SHORT
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue Time Short", ibmmq.MQIACF_Q_TIME_INDICATOR)
	QueueStatus.Attributes[attr].index = 0
	attr = ATTR_Q_QTIME_LONG
	QueueStatus.Attributes[attr] = newStatusAttribute(attr, "Queue Time Long", ibmmq.MQIACF_Q_TIME_INDICATOR)
	QueueStatus.Attributes[attr].index = 1

	qAttrsInit = true

	traceExit("QueueInitAttributes", 0)

}

// If we need to list the queues that match a pattern. Not needed for
// the status queries as they (unlike the pub/sub resource stats) accept
// patterns in the PCF command
func InquireQueues(patterns string) ([]string, error) {
	traceEntry("InquireQueues")
	QueueInitAttributes()
	rc, err := inquireObjects(patterns, ibmmq.MQOT_Q)
	traceExitErr("InquireQueues", 0, err)
	return rc, err
}

func CollectQueueStatus(patterns string) error {
	var err error
	traceEntry("CollectQueueStatus")

	QueueInitAttributes()

	// Empty any collected values
	for k := range QueueStatus.Attributes {
		QueueStatus.Attributes[k].Values = make(map[string]*StatusValue)
	}

	queuePatterns := strings.Split(patterns, ",")
	if len(queuePatterns) == 0 {
		traceExit("CollectQueueStatus", 1)
		return nil
	}

	// If there was a negative pattern, then we have to look through the
	// list of queues and query status individually. Otherwise we can
	// use regular MQ patterns to query queues in a batch.
	if strings.Contains(patterns, "!") {
		for qName, qi := range qInfoMap {
			if len(qName) == 0 || !qi.exists {
				continue
			}
			err = collectQueueStatus(qName, ibmmq.MQOT_Q)
			if err == nil && useResetQStats {
				err = collectResetQStats(qName)
			}
		}
	} else {
		for _, pattern := range queuePatterns {
			pattern = strings.TrimSpace(pattern)
			if len(pattern) == 0 {
				continue
			}

			err = collectQueueStatus(pattern, ibmmq.MQOT_Q)
			if err == nil && useResetQStats {
				err = collectResetQStats(pattern)
			}
		}
	}
	traceExitErr("CollectQueueStatus", 0, err)
	return err
}

// Issue the INQUIRE_QUEUE_STATUS command for a queue or wildcarded queue name
// Collect the responses and build up the statistics
func collectQueueStatus(pattern string, instanceType int32) error {
	var err error
	traceEntryF("collectQueueStatus", "Pattern: %s", pattern)

	statusClearReplyQ()

	putmqmd, pmo, cfh, buf := statusSetCommandHeaders()

	// Can allow all the other fields to default
	cfh.Command = ibmmq.MQCMD_INQUIRE_Q_STATUS

	// Add the parameters one at a time into a buffer
	pcfparm := new(ibmmq.PCFParameter)
	pcfparm.Type = ibmmq.MQCFT_STRING
	pcfparm.Parameter = ibmmq.MQCA_Q_NAME
	pcfparm.String = []string{pattern}
	cfh.ParameterCount++
	buf = append(buf, pcfparm.Bytes()...)

	pcfparm = new(ibmmq.PCFParameter)
	pcfparm.Type = ibmmq.MQCFT_INTEGER
	pcfparm.Parameter = ibmmq.MQIACF_Q_STATUS_TYPE
	pcfparm.Int64Value = []int64{int64(ibmmq.MQIACF_Q_STATUS)}
	cfh.ParameterCount++
	buf = append(buf, pcfparm.Bytes()...)

	// Once we know the total number of parameters, put the
	// CFH header on the front of the buffer.
	buf = append(cfh.Bytes(), buf...)

	// And now put the command to the queue
	err = cmdQObj.Put(putmqmd, pmo, buf)
	if err != nil {
		traceExit("collectQueueStatus", 1)
		return err
	}

	// Now get the responses - loop until all have been received (one
	// per queue) or we run out of time
	for allReceived := false; !allReceived; {
		cfh, buf, allReceived, err = statusGetReply()
		if buf != nil {
			parseQData(instanceType, cfh, buf)
		}
	}

	traceExitErr("collectQueueStatus", 0, err)
	return err
}

func collectResetQStats(pattern string) error {
	var err error

	traceEntry("collectResetQStats")
	statusClearReplyQ()
	putmqmd, pmo, cfh, buf := statusSetCommandHeaders()

	// Can allow all the other fields to default
	cfh.Command = ibmmq.MQCMD_RESET_Q_STATS

	// Add the parameters one at a time into a buffer
	pcfparm := new(ibmmq.PCFParameter)
	pcfparm.Type = ibmmq.MQCFT_STRING
	pcfparm.Parameter = ibmmq.MQCA_Q_NAME
	pcfparm.String = []string{pattern}
	cfh.ParameterCount++
	buf = append(buf, pcfparm.Bytes()...)

	buf = append(cfh.Bytes(), buf...)

	// And now put the command to the queue
	err = cmdQObj.Put(putmqmd, pmo, buf)
	if err != nil {
		traceExitErr("collectResetQueueStats", 1, err)
		return err
	}

	// Now get the responses - loop until all have been received (one
	// per queue) or we run out of time
	for allReceived := false; !allReceived; {
		cfh, buf, allReceived, err = statusGetReply()
		if buf != nil {
			parseResetQStatsData(cfh, buf)
		}
	}
	traceExitErr("collectResetQueueStats", 0, err)
	return err
}

// Issue the INQUIRE_Q call for wildcarded queue names and
// extract the required attributes - currently, just the
// Maximum Queue Depth
func inquireQueueAttributes(objectPatternsList string) error {
	var err error

	traceEntry("inquireQueueAttributes")
	statusClearReplyQ()

	if objectPatternsList == "" {
		traceExitErr("inquireQueueAttributes", 1, err)
		return err
	}

	objectPatterns := strings.Split(strings.TrimSpace(objectPatternsList), ",")
	for i := 0; i < len(objectPatterns) && err == nil; i++ {
		var buf []byte
		pattern := strings.TrimSpace(objectPatterns[i])
		if len(pattern) == 0 {
			continue
		}

		putmqmd, pmo, cfh, buf := statusSetCommandHeaders()

		// Can allow all the other fields to default
		cfh.Command = ibmmq.MQCMD_INQUIRE_Q
		cfh.ParameterCount = 0

		// Add the parameters one at a time into a buffer
		pcfparm := new(ibmmq.PCFParameter)
		pcfparm.Type = ibmmq.MQCFT_STRING
		pcfparm.Parameter = ibmmq.MQCA_Q_NAME
		pcfparm.String = []string{pattern}
		cfh.ParameterCount++
		buf = append(buf, pcfparm.Bytes()...)

		pcfparm = new(ibmmq.PCFParameter)
		pcfparm.Type = ibmmq.MQCFT_INTEGER_LIST
		pcfparm.Parameter = ibmmq.MQIACF_Q_ATTRS
		pcfparm.Int64Value = []int64{int64(ibmmq.MQIA_MAX_Q_DEPTH), int64(ibmmq.MQIA_USAGE), int64(ibmmq.MQCA_Q_DESC)}
		cfh.ParameterCount++
		buf = append(buf, pcfparm.Bytes()...)

		// Once we know the total number of parameters, put the
		// CFH header on the front of the buffer.
		buf = append(cfh.Bytes(), buf...)

		// And now put the command to the queue
		err = cmdQObj.Put(putmqmd, pmo, buf)
		if err != nil {
			traceExitErr("inquireQueueAttributes", 2, err)
			return err
		}

		for allReceived := false; !allReceived; {
			cfh, buf, allReceived, err = statusGetReply()
			if buf != nil {
				parseQAttrData(cfh, buf)
			}
		}
	}
	traceExit("inquireQueueAttributes", 0)
	return nil
}

// Given a PCF response message, parse it to extract the desired statistics
func parseQData(instanceType int32, cfh *ibmmq.MQCFH, buf []byte) string {
	var elem *ibmmq.PCFParameter

	traceEntry("parseQData")
	qName := ""
	key := ""

	lastPutTime := ""
	lastGetTime := ""
	lastPutDate := ""
	lastGetDate := ""

	parmAvail := true
	bytesRead := 0
	offset := 0
	datalen := len(buf)
	if cfh == nil || cfh.ParameterCount == 0 {
		traceExit("parseQData", 1)
		return ""
	}

	// Parse it once to extract the fields that are needed for the map key
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		// Only one field needed for queues
		switch elem.Parameter {
		case ibmmq.MQCA_Q_NAME:
			qName = strings.TrimSpace(elem.String[0])
		}
	}

	// Create a unique key for this instance
	key = qName
	QueueStatus.Attributes[ATTR_Q_NAME].Values[key] = newStatusValueString(qName)

	// And then re-parse the message so we can store the metrics now knowing the map key
	parmAvail = true
	offset = 0
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		if !statusGetIntAttributes(QueueStatus, elem, key) {
			switch elem.Parameter {
			case ibmmq.MQCACF_LAST_PUT_TIME:
				lastPutTime = strings.TrimSpace(elem.String[0])
			case ibmmq.MQCACF_LAST_PUT_DATE:
				lastPutDate = strings.TrimSpace(elem.String[0])
			case ibmmq.MQCACF_LAST_GET_TIME:
				lastGetTime = strings.TrimSpace(elem.String[0])
			case ibmmq.MQCACF_LAST_GET_DATE:
				lastGetDate = strings.TrimSpace(elem.String[0])
			}
		}
	}

	now := time.Now()
	QueueStatus.Attributes[ATTR_Q_SINCE_PUT].Values[key] = newStatusValueInt64(statusTimeDiff(now, lastPutDate, lastPutTime))
	QueueStatus.Attributes[ATTR_Q_SINCE_GET].Values[key] = newStatusValueInt64(statusTimeDiff(now, lastGetDate, lastGetTime))
	if s, ok := qInfoMap[key]; ok {
		maxDepth := s.AttrMaxDepth
		QueueStatus.Attributes[ATTR_Q_MAX_DEPTH].Values[key] = newStatusValueInt64(maxDepth)
		usage := s.AttrUsage
		QueueStatus.Attributes[ATTR_Q_USAGE].Values[key] = newStatusValueInt64(usage)
	}
	traceExitF("parseQData", 0, "Key: %s", key)
	return key
}

// Given a PCF response message, parse it to extract the desired statistics
func parseResetQStatsData(cfh *ibmmq.MQCFH, buf []byte) string {
	var elem *ibmmq.PCFParameter

	traceEntry("parseResetQStatsData")
	qName := ""
	key := ""

	parmAvail := true
	bytesRead := 0
	offset := 0
	datalen := len(buf)
	if cfh == nil || cfh.ParameterCount == 0 {
		traceExit("parseResetQStatsData", 1)
		return ""
	}

	// Parse it once to extract the fields that are needed for the map key
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		// Only one field needed for queues
		switch elem.Parameter {
		case ibmmq.MQCA_Q_NAME:
			qName = strings.TrimSpace(elem.String[0])
		}
	}

	// Create a unique key for this instance
	key = qName

	QueueStatus.Attributes[ATTR_Q_NAME].Values[key] = newStatusValueString(qName)

	// And then re-parse the message so we can store the metrics now knowing the map key
	parmAvail = true
	offset = 0
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		statusGetIntAttributes(QueueStatus, elem, key)
	}

	traceExitF("parseResetQStatsData", 0, "Key: %s", key)
	return key
}

func parseQAttrData(cfh *ibmmq.MQCFH, buf []byte) {
	var elem *ibmmq.PCFParameter
	traceEntry("parseQAttrData")
	qName := ""

	parmAvail := true
	bytesRead := 0
	offset := 0
	datalen := len(buf)
	if cfh.ParameterCount == 0 {
		traceExit("parseQAttrData", 1)
		return
	}
	// Parse it once to extract the fields that are needed for the map key
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		// Only one field needed for queues
		switch elem.Parameter {
		case ibmmq.MQCA_Q_NAME:
			qName = strings.TrimSpace(elem.String[0])
		}
	}

	// And then re-parse the message so we can store the metrics now knowing the map key
	parmAvail = true
	offset = 0
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		switch elem.Parameter {
		case ibmmq.MQIA_MAX_Q_DEPTH:
			v := elem.Int64Value[0]
			if v > 0 {
				if qInfo, ok := qInfoMap[qName]; ok {
					qInfo.AttrMaxDepth = v
				}
			}
			//fmt.Printf("MaxQDepth for %s = %d \n",qName,v)
		case ibmmq.MQIA_USAGE:
			v := elem.Int64Value[0]
			if v > 0 {
				if qInfo, ok := qInfoMap[qName]; ok {
					qInfo.AttrUsage = v
				}
			}
		case ibmmq.MQCA_Q_DESC:
			v := elem.String[0]
			if v != "" {
				if qInfo, ok := qInfoMap[qName]; ok {
					qInfo.Description = v
				}
			}
		}

	}

	traceExit("parseQAttrData", 0)
	return
}

// Return a standardised value.
func QueueNormalise(attr *StatusAttribute, v int64) float64 {
	return statusNormalise(attr, v)
}
