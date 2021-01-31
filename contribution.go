package main

import (
	"context"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/evookelj/inmap/emissions/slca"
	"github.com/evookelj/inmap/emissions/slca/eieio"
	"github.com/evookelj/inmap/emissions/slca/eieio/ces"
	"github.com/evookelj/inmap/emissions/slca/eieio/eieiorpc"
	"github.com/evookelj/inmap/epi"
	"github.com/pkg/errors"
	"gonum.org/v1/gonum/mat"
	"log"
	"os"
)

var CONFIG = os.ExpandEnv("${INMAP_SANDBOX_ROOT}/data/my_config.toml")

func getEIOServer() (*eieio.Server, error) {
	f, err := os.Open(CONFIG)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg eieio.ServerConfig
	_, err = toml.DecodeReader(f, &cfg)
	if err != nil {
		return nil, err
	}
	cfg.Config.Years = []eieio.Year{2003, 2004, 2005, 2006, 2007, 2008, 2009, 2010, 2011, 2012, 2013, 2014, 2015}

	return eieio.NewServer(&cfg, "", epi.NasariACS)
}

// Given an EIEIO server, get the consumption for the specified demographic and year
// organized by SCC
func getConsumptionBySCC(s *eieio.Server, dem *eieiorpc.Demograph, year int32) (*mat.VecDense, error) {
	totalConsRPC, err := s.CES.DemographicConsumption(context.Background(), &eieiorpc.DemographicConsumptionInput{
		Year:      year,
		Demograph: dem,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error calculating demographic consumption")
	}

	consumptionBySCC := make([]float64, len(s.SCCs))
	for industryIdx, consumption := range totalConsRPC.Data {
		SCCs := s.IndustryToSCCMap[industryIdx]
		for _, sccIdx := range SCCs {
			consumptionBySCC[sccIdx] += consumption
		}
	}

	return mat.NewVecDense(len(consumptionBySCC), consumptionBySCC), nil
}

// Get emissions by SCC for the specified year and location
func getEmissionsBySCC(demand *eieiorpc.Vector, s *eieio.Server, year int32, loc eieiorpc.Location) (*mat.VecDense, error) {
	emisRPC, err := s.EmissionsMatrix(context.Background(), &eieiorpc.EmissionsMatrixInput{
		Demand:               demand,
		Year:                 year,
		Location:             loc,
		AQM:                  "isrm",
	})
	if err != nil {
		return nil, errors.Wrap(err, "error getting emissions matrix")
	}
	emis := rpc2mat(emisRPC)

	if _, c := emis.Dims(); c != len(s.SCCs) {
		return nil, fmt.Errorf("expected emissions to have #SCC %d columns, got %d", len(s.SCCs), c)
	}

	// METHOD A: USE VEC
	emisSCC := make([]float64, len(s.SCCs))
	for sectorIdx := range s.SCCs {
		emissionsForSector := emis.ColView(sectorIdx)
		var totalEmissions float64 = 0
		for i := 0; i < emissionsForSector.Len(); i++ {
			totalEmissions += emissionsForSector.AtVec(i)
		}
		emisSCC[sectorIdx] = totalEmissions
	}

	return mat.NewVecDense(len(emisSCC), emisSCC), nil
}

// Return a matrix of emissions by demographic and sector
// along with the rows/columns for that matrix
func demAndEmissions(s *eieio.Server, demand *eieiorpc.Vector, dems []*eieiorpc.Demograph, year int32, loc eieiorpc.Location) (*mat.Dense, []slca.SCC, error) {
	emis, err := getEmissionsBySCC(demand, s, year, loc)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error getting emissions by SCC")
	}

	demAndSec := mat.NewDense(len(dems), len(s.SCCs), nil)
	for demIdx := range dems {
		consumption, err := getConsumptionBySCC(s, dems[demIdx], year)
		if err != nil {
			return nil, nil, errors.Wrap(err, "error getting consumption")
		}

		var manualDot float64 = 0
		for sectorIdx := 0; sectorIdx < consumption.Len(); sectorIdx++ {
			emisForDemAndSCC := consumption.At(sectorIdx, 0) * emis.At(sectorIdx, 0)
			manualDot += emisForDemAndSCC
			demAndSec.Set(demIdx, sectorIdx, emisForDemAndSCC)
		}
	}

	return demAndSec, s.SCCs, nil
}

func getExposureByPopulation(s *eieio.Server, year int32, loc eieiorpc.Location, demand *eieiorpc.Vector) (*map[string]float64, error) {
	vec, err := s.SpatialEIO.Concentrations(context.Background(), &eieiorpc.ConcentrationInput{
		Demand:    demand,
		Pollutant: eieiorpc.Pollutant_TotalPM25,
		Year:      year,
		Location:  loc,
		AQM:       "isrm",
	})
	conc := vec.Data
	if err != nil {
		return nil, err
	}

	populationNamesOutput, err := s.Populations(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	popNames := populationNamesOutput.Names

	populationGridsByPopName := make(map[string][]float64)
	for _, popName := range popNames {
		popOutputStruct, err := s.CSTConfig.PopulationIncidence(context.Background(), &eieiorpc.PopulationIncidenceInput{
			Year:       year,
			Population: popName,
			// these two don't matter b/c we just care about population count
			// TODO: Export method that just gets pop counts, don't waste computing on incidence
			HR:         "NasariACS",
			AQM:        "isrm",
		})
		if err != nil {
			return nil, err
		}

		pop := popOutputStruct.GetPopulation()
		if len(pop) != len(conc) {
			return nil, fmt.Errorf("expected len(population)=len(concentrations); got %d != %d", len(pop), len(conc))
		}
		populationGridsByPopName[popName] = pop
	}

	popTotals := make(map[string]float64)
	for _, pop := range popNames {
		popTotals[pop] = 0
	}

	exposureByPop := make(map[string]float64)
	for gridIdx, concentrationAmt := range conc {
		log.Printf("\t[Grid %d] [Concentration=%.2f]", gridIdx, concentrationAmt)
		for _, popName := range popNames {
			numIndividuals := populationGridsByPopName[popName][gridIdx]
			popTotals[popName] += numIndividuals
			exposureByPop[popName] += numIndividuals * concentrationAmt
			log.Printf("\t\t[Population %s] %.2f ppl --> %.2f exposure", popName, numIndividuals, numIndividuals * concentrationAmt)
		}
	}

	for popName, exposure := range exposureByPop {
		log.Printf("Pop name: %s\tExposure: %.2f", popName, exposure)
	}

	return nil, nil
}

func emissionsAndDemTesting() error {
	/*
	var eths []eieiorpc.Demograph
	for val := 0; val < len(eieiorpc.Ethnicity_value); val++ {
		eth := eieiorpc.Ethnicity(val)
		if eth != eieiorpc.Ethnicity_Ethnicity_All{
			eths = append(eths, *ces.EthnicityToDemograph(eth))
		}
	}
	dems := eths*/

	var deciles []*eieiorpc.Demograph
	for val := 0; val < len(eieiorpc.Decile_value); val++ {
		dec := eieiorpc.Decile(val)
		if dec != eieiorpc.Decile_Decile_All {
			deciles = append(deciles, ces.DecileToDemograph(dec))
		}
	}
	dems := deciles

	s, err := getEIOServer()
	if err != nil {
		return errors.Wrap(err, "error creating EIO server")
	}

	var year int32 = 2015
	loc := eieiorpc.Location_Domestic

	demand, err := s.FinalDemand(context.TODO(), &eieiorpc.FinalDemandInput{
		FinalDemandType: eieiorpc.FinalDemandType_AllDemand,
		Year:            year,
		Location:        loc,
	})
	if err != nil {
		return errors.Wrap(err, "error getting final demand")
	}

	emisByDemAndSCC, _, err := demAndEmissions(s, demand, dems, year, loc)
	if err != nil {
		return err
	}

	err = populationAdjust(s, emisByDemAndSCC, dems)
	if err != nil {
		return err
	}

	for demIdx := range dems {
		var demTotalEmissions float64 = 0
		for _, emisForSCCForDem := range emisByDemAndSCC.RawRowView(demIdx) {
			demTotalEmissions += emisForSCCForDem
		}
		log.Printf("Index: %d\tTotal emissions (pop-adjusted): %.2f", demIdx, demTotalEmissions)
	}

	_, err = getExposureByPopulation(s, year, loc, demand)
	if err != nil {
		return err
	}

	return nil
}

func populationAdjust(s *eieio.Server, emisByDemAndSCC *mat.Dense, dems []*eieiorpc.Demograph) error {
	// multiplying result values by the ratio of the total population count
	// to the population count of the group in question
	totalPop := 0
	popCounts := make([]int, len(dems))
	for demIdx, dem := range dems {
		demCount, err := s.CES.TotalPopulationCount(dem, 2015) // N: hardcoded year
		if err != nil {
			return err
		}
		totalPop += demCount
		popCounts[demIdx] = demCount
	}

	numRows, numCols := emisByDemAndSCC.Dims()
	if numRows != len(dems) {
		return fmt.Errorf("Expected emissions to have length of dem, %d != %d", numRows, len(dems))
	}
	for demIdx := range dems {
		adjustRatio := float64(totalPop)/float64(popCounts[demIdx])
		for j := 0; j < numCols; j++ {
			emisByDemAndSCC.Set(demIdx, j, emisByDemAndSCC.At(demIdx, j) * adjustRatio)
		}
	}
	return nil
}

func main() {
	err := emissionsAndDemTesting()
	if err != nil {
		log.Fatalf(err.Error())
	}
}